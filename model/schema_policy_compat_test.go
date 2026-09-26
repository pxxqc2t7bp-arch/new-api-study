package model

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gormMySQL "gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type legacyOptionWithoutPrimaryKey struct {
	Key   string `gorm:"type:varchar(191)"`
	Value string
}

type legacyOptionWithNullableKey struct {
	Key   *string `gorm:"type:varchar(191);uniqueIndex:idx_options_key"`
	Value string
}

type legacyOptionWithUniqueKey struct {
	Key   string `gorm:"type:varchar(191);uniqueIndex:idx_options_key"`
	Value string
}

type extendedRequiredOption struct {
	Key          string `gorm:"type:varchar(191);primaryKey"`
	Value        string `gorm:"type:varchar(32);not null"`
	SchemaMarker string `gorm:"type:varchar(32);not null"`
}

type legacyMigrationSentinel struct {
	ID     int `gorm:"primaryKey"`
	Marker string
}

type failingOptionEntropyReader struct{}

func (failingOptionEntropyReader) Read([]byte) (int, error) {
	return 0, errors.New("entropy unavailable")
}

type mysqlOptionRecoverySnapshot struct {
	ActiveDDL        string
	BackupDDL        string
	JournalDDL       string
	JournalRows      []mysqlOptionTerminalJournalSnapshot
	ActiveRows       []Option
	BackupRows       []Option
	ActiveRowsSHA256 string
	BackupRowsSHA256 string
	Triggers         []mysqlOptionTrigger
}

type postgres96CatalogDriver struct {
	mu      sync.Mutex
	queries []string
}

var postgres96CatalogDriverSequence atomic.Uint64

func (catalog *postgres96CatalogDriver) Open(string) (driver.Conn, error) {
	return &postgres96CatalogConn{catalog: catalog}, nil
}

func (catalog *postgres96CatalogDriver) record(query string) {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	catalog.queries = append(catalog.queries, query)
}

func (catalog *postgres96CatalogDriver) recordedQueries() []string {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	return slices.Clone(catalog.queries)
}

type postgres96CatalogConn struct {
	catalog *postgres96CatalogDriver
}

func (*postgres96CatalogConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (*postgres96CatalogConn) Close() error {
	return nil
}

func (*postgres96CatalogConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (conn *postgres96CatalogConn) QueryContext(
	_ context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	conn.catalog.record(query)
	for _, unsupported := range []string{
		"pg_catalog.pg_publication_rel",
		"relation.relam",
		"pg_catalog.pg_am",
		"default_table_access_method",
	} {
		if strings.Contains(query, unsupported) {
			return nil, fmt.Errorf("PostgreSQL 9.6 query references unsupported metadata: %s", unsupported)
		}
	}
	switch {
	case strings.TrimSpace(query) == "SHOW server_version_num":
		return &postgres96CatalogRows{
			columns: []string{"server_version_num"},
			values:  [][]driver.Value{{int64(90624)}},
		}, nil
	case strings.Contains(query, "AS has_unsupported_relation_metadata"):
		return &postgres96CatalogRows{
			columns: []string{
				"has_unsupported_relation_metadata",
				"has_foreign_key",
				"has_trigger",
				"has_rls",
				"has_rule",
				"has_dependent_view",
				"has_description",
				"has_acl",
				"has_column_acl",
				"has_other_owner",
				"has_publication",
			},
			values: [][]driver.Value{{
				false, false, false, false, false, false,
				false, false, false, false, false,
			}},
		}, nil
	case strings.Contains(query, "FROM information_schema.columns"):
		normalized := strings.Join(strings.Fields(query), " ")
		if normalized != "SELECT column_name, 'NO' AS is_identity, 'NEVER' AS is_generated,"+
			" pg_catalog.pg_get_serial_sequence( pg_catalog.quote_ident(table_schema) || "+
			"'.' || pg_catalog.quote_ident(table_name), column_name ) IS NOT NULL "+
			"AS has_owned_sequence "+
			"FROM information_schema.columns WHERE table_schema = current_schema() "+
			"AND table_name = $1 ORDER BY ordinal_position" ||
			len(args) != 1 || args[0].Value != "options" {
			return nil, errors.New("PostgreSQL 9.6 query references unsupported column metadata")
		}
		return &postgres96CatalogRows{
			columns: []string{
				"column_name", "is_identity", "is_generated", "has_owned_sequence",
			},
			values: [][]driver.Value{
				{"key", "NO", "NEVER", false},
				{"value", "NO", "NEVER", false},
			},
		}, nil
	default:
		return nil, fmt.Errorf("unexpected PostgreSQL 9.6 catalog query")
	}
}

type postgres96CatalogRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (rows *postgres96CatalogRows) Columns() []string {
	return rows.columns
}

func (*postgres96CatalogRows) Close() error {
	return nil
}

func (rows *postgres96CatalogRows) Next(destination []driver.Value) error {
	if rows.index == len(rows.values) {
		return io.EOF
	}
	copy(destination, rows.values[rows.index])
	rows.index++
	return nil
}

type optionMigrationSnapshotBarrier struct {
	logger.Interface
	once              sync.Once
	statement         string
	additionalMatch   string
	reached           chan struct{}
	continueMigration chan struct{}
}

func (barrier *optionMigrationSnapshotBarrier) Trace(
	ctx context.Context,
	begin time.Time,
	sql func() (string, int64),
	err error,
) {
	statement, _ := sql()
	if strings.Contains(statement, barrier.statement) &&
		(barrier.additionalMatch == "" || strings.Contains(statement, barrier.additionalMatch)) {
		barrier.once.Do(func() {
			close(barrier.reached)
			<-barrier.continueMigration
		})
	}
	barrier.Interface.Trace(ctx, begin, sql, err)
}

type optionMigrationCrashLogger struct {
	logger.Interface
	readyPath    string
	continuePath string
}

func (crash *optionMigrationCrashLogger) Trace(
	ctx context.Context,
	begin time.Time,
	sql func() (string, int64),
	err error,
) {
	statement, _ := sql()
	crash.Interface.Trace(ctx, begin, sql, err)
	if strings.Contains(statement, "UNLOCK TABLES") {
		if writeErr := os.WriteFile(crash.readyPath, []byte("ready"), 0o600); writeErr != nil {
			os.Exit(87)
		}
		for {
			if _, statErr := os.Stat(crash.continuePath); statErr == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if strings.Contains(statement, "RENAME TABLE `options` TO `options_legacy_") {
		os.Exit(86)
	}
}

type optionMigrationFinalizationCrashLogger struct {
	logger.Interface
	statement string
}

func (crash *optionMigrationFinalizationCrashLogger) Trace(
	ctx context.Context,
	begin time.Time,
	sql func() (string, int64),
	err error,
) {
	statement, _ := sql()
	crash.Interface.Trace(ctx, begin, sql, err)
	if strings.Contains(statement, crash.statement) {
		os.Exit(85)
	}
}

type optionMigrationTerminalValidationBarrier struct {
	logger.Interface
	armed             atomic.Bool
	once              sync.Once
	reached           chan struct{}
	continueMigration chan struct{}
}

func (barrier *optionMigrationTerminalValidationBarrier) Trace(
	ctx context.Context,
	begin time.Time,
	sql func() (string, int64),
	err error,
) {
	statement, _ := sql()
	if strings.Contains(statement, "DROP COLUMN `"+optionMigrationStateCol+"`") {
		barrier.armed.Store(true)
	}
	if barrier.armed.Load() && strings.Contains(statement, "AS has_incoming_foreign_key") {
		barrier.once.Do(func() {
			close(barrier.reached)
			<-barrier.continueMigration
		})
	}
	barrier.Interface.Trace(ctx, begin, sql, err)
}

func useMigrationTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := openAppCredentialTestDB(t)
	previousDB := DB
	DB = db
	initCol()
	t.Cleanup(func() {
		DB = previousDB
		initCol()
	})
	return db
}

func captureMySQLOptionRecoverySnapshot(
	t *testing.T,
	db *gorm.DB,
	backup string,
) mysqlOptionRecoverySnapshot {
	t.Helper()
	activeDDL, err := showCreateMySQLTable(db, "options")
	require.NoError(t, err)
	backupDDL, err := showCreateMySQLTable(db, backup)
	require.NoError(t, err)
	var journalDDL string
	var journalRows []mysqlOptionTerminalJournalSnapshot
	if mysqlOptionTerminalJournalExists(t, db) {
		journalDDL, err = showCreateMySQLTable(db, optionTerminalJournalTable)
		require.NoError(t, err)
		require.NoError(t, db.Table(optionTerminalJournalTable).
			Select(
				"singleton",
				"active_rows_sha256",
				"retained_rows_sha256",
				"retained_canonical_rows_sha256",
			).
			Order("singleton").Find(&journalRows).Error)
	}
	var activeRows, backupRows []Option
	require.NoError(t, db.Table("options").Order("`key`").Find(&activeRows).Error)
	require.NoError(t, db.Table(backup).Order("`key`").Find(&backupRows).Error)
	activeColumns, err := mysqlWritableOptionColumnsFromTable(db, "options")
	require.NoError(t, err)
	activeRowsSHA256, err := mysqlOptionRowsDigest(db, "options", activeColumns)
	require.NoError(t, err)
	backupColumns, err := mysqlWritableOptionColumnsFromTable(db, backup)
	require.NoError(t, err)
	backupRowsSHA256, err := mysqlOptionRowsDigest(db, backup, backupColumns)
	require.NoError(t, err)
	var triggers []mysqlOptionTrigger
	require.NoError(t, db.Raw(`
SELECT TRIGGER_NAME AS trigger_name,
       EVENT_MANIPULATION AS event_manipulation,
       ACTION_TIMING AS action_timing,
       EVENT_OBJECT_TABLE AS event_object_table,
       ACTION_STATEMENT AS action_statement
FROM information_schema.triggers
WHERE trigger_schema = DATABASE() AND event_object_table IN (?, ?)
ORDER BY trigger_name`, "options", backup).Scan(&triggers).Error)
	return mysqlOptionRecoverySnapshot{
		ActiveDDL:        activeDDL,
		BackupDDL:        backupDDL,
		JournalDDL:       journalDDL,
		JournalRows:      journalRows,
		ActiveRows:       activeRows,
		BackupRows:       backupRows,
		ActiveRowsSHA256: activeRowsSHA256,
		BackupRowsSHA256: backupRowsSHA256,
		Triggers:         triggers,
	}
}

func crashMySQLOptionMigrationAfterSwap(t *testing.T, db *gorm.DB) string {
	t.Helper()
	var databaseName string
	require.NoError(t, db.Raw("SELECT DATABASE()").Scan(&databaseName).Error)
	parsed, err := mysqlDriver.ParseDSN(os.Getenv("APP_PLUGIN_TEST_DSN"))
	require.NoError(t, err)
	parsed.DBName = databaseName
	coordination := t.TempDir()
	readyPath := filepath.Join(coordination, "unlocked")
	continuePath := filepath.Join(coordination, "continue")
	executable, err := os.Executable()
	require.NoError(t, err)
	command := exec.CommandContext(
		t.Context(),
		executable,
		"-test.run=^TestOptionPrimaryKeyMigrationRecoversPostSwapMySQLCrash$",
		"-test.v",
	)
	command.Env = append(os.Environ(),
		"OPTION_MIGRATION_POST_SWAP_CRASH_HELPER=1",
		"OPTION_MIGRATION_POST_SWAP_CRASH_DSN="+parsed.FormatDSN(),
		"OPTION_MIGRATION_POST_SWAP_CRASH_READY="+readyPath,
		"OPTION_MIGRATION_POST_SWAP_CRASH_GO="+continuePath,
	)
	require.NoError(t, command.Start())
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(readyPath)
		return statErr == nil
	}, 10*time.Second, 10*time.Millisecond, "migration did not expose the post-unlock window")
	require.NoError(t, os.WriteFile(continuePath, []byte("continue"), 0o600))
	waitErr := command.Wait()
	var exitErr *exec.ExitError
	require.ErrorAs(t, waitErr, &exitErr)
	require.Equal(t, 86, exitErr.ExitCode(), "crash helper must stop immediately after the atomic swap")

	var marker string
	require.NoError(t, db.Raw(`
SELECT column_comment
FROM information_schema.columns
WHERE table_schema = DATABASE() AND table_name = 'options' AND column_name = ?`,
		optionMigrationMarkerCol).Scan(&marker).Error)
	artifacts, ok := parseMySQLOptionMigrationArtifacts("options", marker)
	require.True(t, ok)
	backup := optionLegacyTablePrefix + artifacts.ID
	require.True(t, db.Migrator().HasTable(backup))
	return backup
}

func crashMySQLOptionFinalizationAt(t *testing.T, db *gorm.DB, statement string) {
	t.Helper()
	var databaseName string
	require.NoError(t, db.Raw("SELECT DATABASE()").Scan(&databaseName).Error)
	parsed, err := mysqlDriver.ParseDSN(os.Getenv("APP_PLUGIN_TEST_DSN"))
	require.NoError(t, err)
	parsed.DBName = databaseName
	executable, err := os.Executable()
	require.NoError(t, err)
	command := exec.CommandContext(
		t.Context(),
		executable,
		"-test.run=^TestOptionPrimaryKeyMigrationFinalizationCrashProtocol$",
		"-test.v",
	)
	command.Env = append(os.Environ(),
		"OPTION_MIGRATION_FINALIZATION_CRASH_HELPER=1",
		"OPTION_MIGRATION_FINALIZATION_CRASH_DSN="+parsed.FormatDSN(),
		"OPTION_MIGRATION_FINALIZATION_CRASH_STATEMENT="+statement,
	)
	waitErr := command.Run()
	var exitErr *exec.ExitError
	require.ErrorAs(t, waitErr, &exitErr)
	require.Equal(t, 85, exitErr.ExitCode(), "crash helper must stop after the target finalization DDL")
}

func mysqlOptionTerminalRetainedTable(t *testing.T, db *gorm.DB) string {
	t.Helper()
	var tables []string
	require.NoError(t, db.Raw(`
SELECT table_name
FROM information_schema.tables
WHERE table_schema = DATABASE() AND table_name LIKE ?
ORDER BY table_name`, optionLegacyTablePrefix+"%").Scan(&tables).Error)
	require.Len(t, tables, 1)
	require.True(t, isOptionLegacyTable(tables[0]))
	return tables[0]
}

func mysqlOptionTerminalArtifactID(t *testing.T, retained string) string {
	t.Helper()
	id, ok := strings.CutPrefix(retained, optionLegacyTablePrefix)
	require.True(t, ok)
	require.True(t, isMySQLOptionMigrationArtifactID(id))
	return id
}

func mysqlOptionTerminalJournalExists(t *testing.T, db *gorm.DB) bool {
	t.Helper()
	var count int64
	require.NoError(t, db.Raw(`
SELECT count(*)
FROM information_schema.tables
WHERE table_schema = DATABASE() AND table_name = ?`,
		optionTerminalJournalTable).Scan(&count).Error)
	return count != 0
}

func crashMySQLOptionRestoreAt(
	t *testing.T,
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	initialState mysqlOptionMigrationState,
	point mysqlOptionFinalizationRestorePoint,
	statement string,
) {
	t.Helper()
	var databaseName string
	require.NoError(t, db.Raw("SELECT DATABASE()").Scan(&databaseName).Error)
	parsed, err := mysqlDriver.ParseDSN(os.Getenv("APP_PLUGIN_TEST_DSN"))
	require.NoError(t, err)
	parsed.DBName = databaseName
	executable, err := os.Executable()
	require.NoError(t, err)
	command := exec.CommandContext(
		t.Context(),
		executable,
		"-test.run=^TestOptionPrimaryKeyMigrationRestoreCrashRestartResumesAfterEveryDDL$",
		"-test.v",
	)
	command.Env = append(os.Environ(),
		"OPTION_MIGRATION_RESTORE_CRASH_HELPER=1",
		"OPTION_MIGRATION_RESTORE_CRASH_DSN="+parsed.FormatDSN(),
		"OPTION_MIGRATION_RESTORE_CRASH_MARKER="+artifacts.Marker,
		"OPTION_MIGRATION_RESTORE_CRASH_SOURCE_DIGEST="+initialState.SourceSchemaSHA256,
		"OPTION_MIGRATION_RESTORE_CRASH_ACTIVE_DIGEST="+initialState.ActiveSchemaSHA256,
		"OPTION_MIGRATION_RESTORE_CRASH_INITIAL_PHASE="+initialState.Phase,
		"OPTION_MIGRATION_RESTORE_CRASH_STATE_PHASE="+point.StatePhase,
		"OPTION_MIGRATION_RESTORE_CRASH_BRIDGE_PHASE="+point.BridgePhase,
		"OPTION_MIGRATION_RESTORE_CRASH_TERMINAL_DIGEST="+point.TerminalSchemaSHA256,
		"OPTION_MIGRATION_RESTORE_CRASH_STATEMENT="+statement,
	)
	if point.MarkersDropped {
		command.Env = append(command.Env, "OPTION_MIGRATION_RESTORE_CRASH_MARKERS_DROPPED=1")
	}
	waitErr := command.Run()
	var exitErr *exec.ExitError
	require.ErrorAs(t, waitErr, &exitErr)
	require.Equal(t, 85, exitErr.ExitCode(), "crash helper must stop after the target restore DDL")
}

func createLegacyOptions(t *testing.T, db *gorm.DB, rows []legacyOptionWithoutPrimaryKey) {
	t.Helper()
	require.NoError(t, db.Table("options").Migrator().CreateTable(&legacyOptionWithoutPrimaryKey{}))
	require.NoError(t, db.Table("options").Create(&rows).Error)
}

func mysqlCrossSchemaOptionReferenceNames(
	t *testing.T,
	db *gorm.DB,
	stem string,
) (string, string, string) {
	t.Helper()
	var currentSchema string
	require.NoError(t, db.Raw("SELECT DATABASE()").Scan(&currentSchema).Error)
	schema := currentSchema + "_external"
	table := "options_" + stem + "_reference"
	constraint := "fk_options_" + stem + "_reference"
	require.True(t, optionSafeIdent(schema))
	require.True(t, optionSafeIdent(table))
	require.True(t, optionSafeIdent(constraint))
	return schema, table, constraint
}

func createMySQLCrossSchemaOptionReference(
	t *testing.T,
	db *gorm.DB,
	stem string,
	targetTable string,
) string {
	t.Helper()
	schema, table, constraint := mysqlCrossSchemaOptionReferenceNames(t, db, stem)
	var currentSchema string
	require.NoError(t, db.Raw("SELECT DATABASE()").Scan(&currentSchema).Error)
	require.NoError(t, db.Exec(
		"CREATE DATABASE "+quoteMySQLIdent(schema)+
			" CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci",
	).Error)
	t.Cleanup(func() {
		assert.NoError(t, db.Exec("DROP DATABASE IF EXISTS "+quoteMySQLIdent(schema)).Error)
	})
	require.NoError(t, db.Exec(
		"CREATE TABLE "+quoteMySQLIdent(schema)+"."+quoteMySQLIdent(table)+" ("+
			"`id` bigint PRIMARY KEY, "+
			"`option_key` varchar(191) CHARACTER SET utf8mb4 "+
			"COLLATE utf8mb4_unicode_ci NOT NULL, "+
			"CONSTRAINT "+quoteMySQLIdent(constraint)+" "+
			"FOREIGN KEY (`option_key`) REFERENCES "+quoteMySQLIdent(currentSchema)+"."+
			quoteMySQLIdent(targetTable)+" (`key`)"+
			") ENGINE=InnoDB",
	).Error)
	return captureMySQLCrossSchemaOptionReference(t, db, stem)
}

func captureMySQLCrossSchemaOptionReference(t *testing.T, db *gorm.DB, stem string) string {
	t.Helper()
	schema, table, constraint := mysqlCrossSchemaOptionReferenceNames(t, db, stem)
	var tableName, ddl string
	require.NoError(t, db.Raw(
		"SHOW CREATE TABLE "+quoteMySQLIdent(schema)+"."+quoteMySQLIdent(table),
	).Row().Scan(&tableName, &ddl))
	var foreignKey struct {
		ConstraintSchema      string `gorm:"column:constraint_schema"`
		TableSchema           string `gorm:"column:table_schema"`
		ReferencedTableSchema string `gorm:"column:referenced_table_schema"`
		ReferencedTableName   string `gorm:"column:referenced_table_name"`
	}
	require.NoError(t, db.Raw(`
SELECT constraint_schema, table_schema, referenced_table_schema, referenced_table_name
FROM information_schema.key_column_usage
WHERE constraint_schema = ? AND constraint_name = ?`,
		schema, constraint).Scan(&foreignKey).Error)
	require.Equal(t, schema, foreignKey.ConstraintSchema)
	require.Equal(t, schema, foreignKey.TableSchema)
	require.NotEmpty(t, foreignKey.ReferencedTableSchema)
	require.NotEmpty(t, foreignKey.ReferencedTableName)
	return strings.Join([]string{
		tableName,
		ddl,
		foreignKey.ConstraintSchema,
		foreignKey.TableSchema,
		foreignKey.ReferencedTableSchema,
		foreignKey.ReferencedTableName,
	}, "|")
}

func mysqlCrossSchemaOptionReferenceTarget(
	t *testing.T,
	db *gorm.DB,
	stem string,
) (string, string) {
	t.Helper()
	schema, _, constraint := mysqlCrossSchemaOptionReferenceNames(t, db, stem)
	var target struct {
		Schema string `gorm:"column:referenced_table_schema"`
		Table  string `gorm:"column:referenced_table_name"`
	}
	require.NoError(t, db.Raw(`
SELECT referenced_table_schema, referenced_table_name
FROM information_schema.key_column_usage
WHERE constraint_schema = ? AND constraint_name = ?`,
		schema, constraint).Scan(&target).Error)
	require.NotEmpty(t, target.Schema)
	require.NotEmpty(t, target.Table)
	return target.Schema, target.Table
}

type mysqlOptionMigrationAccount struct {
	admin    *gorm.DB
	database string
	name     string
	password string
}

func newMySQLOptionMigrationAccount(t *testing.T, admin *gorm.DB) mysqlOptionMigrationAccount {
	t.Helper()
	var database string
	require.NoError(t, admin.Raw("SELECT DATABASE()").Scan(&database).Error)
	var random [12]byte
	_, err := cryptorand.Read(random[:])
	require.NoError(t, err)
	suffix := hex.EncodeToString(random[:])
	account := mysqlOptionMigrationAccount{
		admin:    admin,
		database: database,
		name:     "migration_" + suffix[:16],
		password: suffix,
	}
	principal := "'" + account.name + "'@'%'"
	require.NoError(t, admin.Exec(
		"CREATE USER "+principal+" IDENTIFIED BY '"+account.password+"'",
	).Error)
	require.NoError(t, admin.Exec(
		"GRANT ALL PRIVILEGES ON "+quoteMySQLIdent(database)+".* TO "+principal,
	).Error)
	t.Cleanup(func() {
		assert.NoError(t, admin.Exec("DROP USER IF EXISTS "+principal).Error)
	})
	return account
}

func (account mysqlOptionMigrationAccount) grantGlobal(t *testing.T, privilege string) {
	t.Helper()
	require.Contains(t, []string{"REFERENCES", "SELECT"}, privilege)
	require.NoError(t, account.admin.Exec(
		"GRANT "+privilege+" ON *.* TO '"+account.name+"'@'%'",
	).Error)
}

func (account mysqlOptionMigrationAccount) open(t *testing.T) *gorm.DB {
	t.Helper()
	parsed, err := mysqlDriver.ParseDSN(os.Getenv("APP_PLUGIN_TEST_DSN"))
	require.NoError(t, err)
	parsed.User = account.name
	parsed.Passwd = account.password
	parsed.DBName = account.database
	db, err := gorm.Open(
		gormMySQL.Open(parsed.FormatDSN()),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func mysqlGlobalMetadataPrivilegeCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Raw(`
SELECT count(*)
FROM information_schema.user_privileges
WHERE grantee = CONCAT(
  '''', SUBSTRING_INDEX(CURRENT_USER(), '@', 1),
  '''@''', SUBSTRING_INDEX(CURRENT_USER(), '@', -1), ''''
)
  AND privilege_type IN ('REFERENCES', 'SELECT')`).Scan(&count).Error)
	return count
}

func assertMySQLOptionMigrationDidNotStart(t *testing.T, db *gorm.DB, expectedDDL string) {
	t.Helper()
	currentDDL, err := showCreateMySQLTable(db, "options")
	require.NoError(t, err)
	assert.Equal(t, expectedDDL, currentDDL)
	var artifactTables int64
	require.NoError(t, db.Raw(`
SELECT count(*)
FROM information_schema.tables
WHERE table_schema = DATABASE()
  AND (
    table_name LIKE ?
    OR table_name LIKE ?
  )`,
		optionMySQLTmpPrefix+"%", optionLegacyTablePrefix+"%",
	).Scan(&artifactTables).Error)
	assert.Zero(t, artifactTables)
	assertNoMySQLOptionMigrationArtifacts(t, db)
}

func TestMySQLOptionMigrationRequiresGlobalMetadataVisibility(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 metadata visibility regression")
	}
	createOptions := func(t *testing.T, db *gorm.DB) string {
		t.Helper()
		require.NoError(t, db.Exec(
			"CREATE TABLE options ("+
				"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
				"`value` longtext,"+
				"UNIQUE KEY `uniq_options_key` (`key`)"+
				") ENGINE=InnoDB",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
			"keep.option", "preserved",
		).Error)
		ddl, err := showCreateMySQLTable(db, "options")
		require.NoError(t, err)
		return ddl
	}

	t.Run("restricted without external foreign key stops before DDL", func(t *testing.T) {
		admin := useMigrationTestDB(t)
		sourceDDL := createOptions(t, admin)
		account := newMySQLOptionMigrationAccount(t, admin)
		restricted := account.open(t)
		require.Zero(t, mysqlGlobalMetadataPrivilegeCount(t, restricted))
		require.ErrorIs(
			t,
			validateMySQLGlobalTableMetadataVisibility(restricted),
			errMySQLGlobalMetadataVisibilityRequired,
		)

		err := migrateOptionPrimaryKey(restricted)

		require.ErrorContains(t, err, "global table metadata visibility")
		assert.NotContains(t, err.Error(), account.name)
		assert.NotContains(t, err.Error(), account.password)
		assertMySQLOptionMigrationDidNotStart(t, admin, sourceDDL)
	})

	t.Run("restricted account accepts an already terminal schema", func(t *testing.T) {
		admin := useMigrationTestDB(t)
		require.NoError(t, admin.Exec(
			"CREATE TABLE options ("+
				"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL PRIMARY KEY,"+
				"`value` longtext"+
				") ENGINE=InnoDB",
		).Error)
		require.NoError(t, admin.Exec(
			"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
			"keep.option", "preserved",
		).Error)
		sourceDDL, err := showCreateMySQLTable(admin, "options")
		require.NoError(t, err)
		account := newMySQLOptionMigrationAccount(t, admin)
		restricted := account.open(t)
		require.Zero(t, mysqlGlobalMetadataPrivilegeCount(t, restricted))

		require.NoError(t, migrateOptionPrimaryKey(restricted))

		currentDDL, err := showCreateMySQLTable(admin, "options")
		require.NoError(t, err)
		assert.Equal(t, sourceDDL, currentDDL)
	})

	t.Run("restricted hidden incoming foreign key stops before DDL", func(t *testing.T) {
		admin := useMigrationTestDB(t)
		sourceDDL := createOptions(t, admin)
		externalDefinition := createMySQLCrossSchemaOptionReference(t, admin, "restricted", "options")
		account := newMySQLOptionMigrationAccount(t, admin)
		restricted := account.open(t)
		var visibleIncoming int64
		require.NoError(t, restricted.Raw(`
SELECT count(*)
FROM information_schema.key_column_usage
WHERE referenced_table_schema = DATABASE()
  AND referenced_table_name = 'options'`).Scan(&visibleIncoming).Error)
		require.Zero(t, visibleIncoming, "the regression requires MySQL 5.7 to hide the external FK")
		require.Zero(t, mysqlGlobalMetadataPrivilegeCount(t, restricted))
		require.ErrorIs(
			t,
			validateMySQLGlobalTableMetadataVisibility(restricted),
			errMySQLGlobalMetadataVisibilityRequired,
		)

		err := migrateOptionPrimaryKey(restricted)

		require.ErrorContains(t, err, "global table metadata visibility")
		assert.NotContains(t, err.Error(), account.name)
		assert.NotContains(t, err.Error(), account.password)
		assertMySQLOptionMigrationDidNotStart(t, admin, sourceDDL)
		assert.Equal(t, externalDefinition, captureMySQLCrossSchemaOptionReference(t, admin, "restricted"))
		targetSchema, targetTable := mysqlCrossSchemaOptionReferenceTarget(t, admin, "restricted")
		assert.Equal(t, account.database, targetSchema)
		assert.Equal(t, "options", targetTable)
	})

	t.Run("global references reconnect sees hidden incoming foreign key", func(t *testing.T) {
		admin := useMigrationTestDB(t)
		sourceDDL := createOptions(t, admin)
		externalDefinition := createMySQLCrossSchemaOptionReference(t, admin, "references", "options")
		account := newMySQLOptionMigrationAccount(t, admin)
		restricted := account.open(t)
		require.Zero(t, mysqlGlobalMetadataPrivilegeCount(t, restricted))
		var hiddenIncoming int64
		require.NoError(t, restricted.Raw(`
SELECT count(*)
FROM information_schema.key_column_usage
WHERE referenced_table_schema = DATABASE()
  AND referenced_table_name = 'options'`).Scan(&hiddenIncoming).Error)
		require.Zero(t, hiddenIncoming)
		restrictedSQL, err := restricted.DB()
		require.NoError(t, err)
		require.NoError(t, restrictedSQL.Close())

		account.grantGlobal(t, "REFERENCES")
		withReferences := account.open(t)
		require.EqualValues(t, 1, mysqlGlobalMetadataPrivilegeCount(t, withReferences))
		require.NoError(t, validateMySQLGlobalTableMetadataVisibility(withReferences))
		var visibleIncoming int64
		require.NoError(t, withReferences.Raw(`
SELECT count(*)
FROM information_schema.key_column_usage
WHERE referenced_table_schema = DATABASE()
  AND referenced_table_name = 'options'`).Scan(&visibleIncoming).Error)
		require.EqualValues(t, 1, visibleIncoming)

		err = migrateOptionPrimaryKey(withReferences)

		require.ErrorContains(t, err, "MySQL options schema cannot be preserved safely")
		assertMySQLOptionMigrationDidNotStart(t, admin, sourceDDL)
		assert.Equal(t, externalDefinition, captureMySQLCrossSchemaOptionReference(t, admin, "references"))
	})

	t.Run("global select permits migration without incoming foreign key", func(t *testing.T) {
		admin := useMigrationTestDB(t)
		createOptions(t, admin)
		account := newMySQLOptionMigrationAccount(t, admin)
		account.grantGlobal(t, "SELECT")
		withSelect := account.open(t)
		require.EqualValues(t, 1, mysqlGlobalMetadataPrivilegeCount(t, withSelect))
		require.NoError(t, validateMySQLGlobalTableMetadataVisibility(withSelect))

		require.NoError(t, migrateOptionPrimaryKey(withSelect))

		primary, err := optionsKeyIsPrimary(admin)
		require.NoError(t, err)
		assert.True(t, primary)
		assertNoMySQLOptionMigrationArtifacts(t, admin)
	})

	t.Run("capability query error fails closed before DDL", func(t *testing.T) {
		db := useMigrationTestDB(t)
		sourceDDL := createOptions(t, db)
		injected := errors.New("injected metadata visibility failure")
		var failed atomic.Bool
		const callbackName = "test:mysql-metadata-visibility-failure"
		require.NoError(t, db.Callback().Row().Before("gorm:row").Register(callbackName, func(tx *gorm.DB) {
			if strings.Contains(tx.Statement.SQL.String(), "information_schema.user_privileges") &&
				failed.CompareAndSwap(false, true) {
				tx.AddError(injected)
			}
		}))
		t.Cleanup(func() {
			require.NoError(t, db.Callback().Row().Remove(callbackName))
		})

		err := migrateOptionPrimaryKey(db)

		require.True(t, failed.Load(), "the capability query must be exercised")
		require.ErrorContains(t, err, "inspect MySQL metadata visibility capability")
		assert.NotContains(t, err.Error(), injected.Error())
		assertMySQLOptionMigrationDidNotStart(t, db, sourceDDL)
	})

	t.Run("restricted account does not clean prepared artifacts", func(t *testing.T) {
		admin := useMigrationTestDB(t)
		sourceDDL := createOptions(t, admin)
		sourceDigest := sha256.Sum256([]byte(sourceDDL))
		artifacts, err := newMySQLOptionMigrationArtifacts(cryptorand.Reader)
		require.NoError(t, err)
		artifacts.SourceSchemaSHA256 = hex.EncodeToString(sourceDigest[:])
		require.NoError(t, createOwnedMySQLOptionMigrationTable(admin, sourceDDL, artifacts))
		require.NoError(t, admin.Exec(
			"ALTER TABLE "+quoteMySQLIdent(artifacts.TmpTable)+" ADD PRIMARY KEY (`key`)",
		).Error)
		require.NoError(t, recordMySQLOptionActiveSchema(admin, &artifacts))
		columns, err := mysqlWritableOptionColumns(admin)
		require.NoError(t, err)
		require.NoError(t, createMySQLOptionMigrationTriggers(admin, columns, artifacts))
		preparedDDL, err := showCreateMySQLTable(admin, artifacts.TmpTable)
		require.NoError(t, err)
		preparedTriggers, err := inspectOwnedMySQLOptionMigrationTriggers(admin, artifacts)
		require.NoError(t, err)
		require.Len(t, preparedTriggers, 3)

		account := newMySQLOptionMigrationAccount(t, admin)
		restricted := account.open(t)
		migrationErr := migrateOptionPrimaryKey(restricted)

		require.ErrorContains(t, migrationErr, "global table metadata visibility")
		currentDDL, err := showCreateMySQLTable(admin, artifacts.TmpTable)
		require.NoError(t, err)
		assert.Equal(t, preparedDDL, currentDDL)
		currentTriggers, err := inspectOwnedMySQLOptionMigrationTriggers(admin, artifacts)
		require.NoError(t, err)
		assert.Equal(t, preparedTriggers, currentTriggers)
		currentSourceDDL, err := showCreateMySQLTable(admin, "options")
		require.NoError(t, err)
		assert.Equal(t, sourceDDL, currentSourceDDL)
		var retainedTables int64
		require.NoError(t, admin.Raw(`
SELECT count(*)
FROM information_schema.tables
WHERE table_schema = DATABASE() AND table_name LIKE ?`,
			optionLegacyTablePrefix+"%",
		).Scan(&retainedTables).Error)
		assert.Zero(t, retainedTables)
	})
}

func TestOptionPrimaryKeyMigrationRejectsMySQLArtifactNameCollisions(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 artifact ownership regression")
	}
	t.Run("same trigger name on unrelated table", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(
			"CREATE TABLE options (`key` varchar(191), `value` longtext) ENGINE=InnoDB",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
		).Error)
		require.NoError(t, db.Exec(
			"CREATE TABLE business_options (`id` bigint PRIMARY KEY, `value` longtext) ENGINE=InnoDB",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO business_options (`id`, `value`) VALUES (1, 'business-preserved')",
		).Error)
		require.NoError(t, db.Exec(
			"CREATE TRIGGER `"+optionInsertTrigger+"` AFTER INSERT ON business_options "+
				"FOR EACH ROW SET @business_option_probe = NEW.`id`",
		).Error)

		err := migrateOptionPrimaryKey(db)

		require.ErrorContains(t, err, "options migration artifact collision")
		var triggerTable string
		require.NoError(t, db.Raw(`
SELECT EVENT_OBJECT_TABLE
FROM information_schema.triggers
WHERE trigger_schema = DATABASE() AND trigger_name = ?`, optionInsertTrigger).
			Scan(&triggerTable).Error)
		assert.Equal(t, "business_options", triggerTable)
		var businessValue string
		require.NoError(t, db.Table("business_options").Select("value").
			Where("id = 1").Scan(&businessValue).Error)
		assert.Equal(t, "business-preserved", businessValue)
		var sourceValue string
		require.NoError(t, db.Table("options").Select("value").
			Where("`key` = ?", "source.option").Scan(&sourceValue).Error)
		assert.Equal(t, "preserved", sourceValue)
	})

	t.Run("same temporary table name belongs to application", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(
			"CREATE TABLE options (`key` varchar(191), `value` longtext) ENGINE=InnoDB",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
		).Error)
		require.NoError(t, db.Exec(
			"CREATE TABLE options_pk_tmp (`id` bigint PRIMARY KEY, `payload` longtext) ENGINE=InnoDB",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options_pk_tmp (`id`, `payload`) VALUES (1, 'business-preserved')",
		).Error)

		err := migrateOptionPrimaryKey(db)

		require.ErrorContains(t, err, "options migration artifact collision")
		var businessValue string
		require.NoError(t, db.Table(optionPrimaryKeyTmpTable).Select("payload").
			Where("id = 1").Scan(&businessValue).Error)
		assert.Equal(t, "business-preserved", businessValue)
		var sourceValue string
		require.NoError(t, db.Table("options").Select("value").
			Where("`key` = ?", "source.option").Scan(&sourceValue).Error)
		assert.Equal(t, "preserved", sourceValue)
	})

	t.Run("same completion journal name belongs to application", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(
			"CREATE TABLE options (`key` varchar(191), `value` longtext) ENGINE=InnoDB",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
		).Error)
		require.NoError(t, db.Exec(
			"CREATE TABLE "+quoteMySQLIdent(optionTerminalJournalTable)+
				" (`id` bigint PRIMARY KEY, `payload` longtext) ENGINE=InnoDB",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO "+quoteMySQLIdent(optionTerminalJournalTable)+
				" (`id`, `payload`) VALUES (1, 'business-preserved')",
		).Error)
		recorder := &migrationSQLRecorder{}

		err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

		require.ErrorIs(t, err, errMySQLOptionTerminalJournalRecoveryBlocked)
		assert.Empty(t, recorder.schemaMutations(), "journal collision must fail before migration DDL")
		var businessValue string
		require.NoError(t, db.Table(optionTerminalJournalTable).Select("payload").
			Where("id = 1").Scan(&businessValue).Error)
		assert.Equal(t, "business-preserved", businessValue)
		var sourceValue string
		require.NoError(t, db.Table("options").Select("value").
			Where("`key` = ?", "source.option").Scan(&sourceValue).Error)
		assert.Equal(t, "preserved", sourceValue)
	})
}

func TestIsMySQLOptionMigrationTriggerName(t *testing.T) {
	validID := strings.Repeat("a", optionArtifactIDSize*2)
	for _, test := range []struct {
		name string
		want bool
	}{
		{name: optionInsertTrigger, want: true},
		{name: optionUpdateTrigger, want: true},
		{name: optionDeleteTrigger, want: true},
		{name: optionInsertTriggerPrefix + validID, want: true},
		{name: optionUpdateTriggerPrefix + validID, want: true},
		{name: optionDeleteTriggerPrefix + validID, want: true},
		{name: optionInsertTriggerPrefix + "audit"},
		{name: optionUpdateTriggerPrefix + validID + "_audit"},
		{name: optionDeleteTriggerPrefix + strings.ToUpper(validID)},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, isMySQLOptionMigrationTriggerName(test.name))
		})
	}
}

func TestOptionPrimaryKeyMigrationClassifiesGeneratedMySQLTriggerNamesExactly(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 exact trigger ownership regression")
	}
	t.Run("near-prefix application trigger is preserved", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(
			"CREATE TABLE options (`key` varchar(191), `value` longtext) ENGINE=InnoDB",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
		).Error)
		require.NoError(t, db.Exec(
			"CREATE TABLE option_audit (`id` bigint PRIMARY KEY) ENGINE=InnoDB",
		).Error)
		const trigger = "options_pk_migrate_ai_audit"
		require.NoError(t, db.Exec(
			"CREATE TRIGGER "+quoteMySQLIdent(trigger)+" AFTER INSERT ON option_audit "+
				"FOR EACH ROW SET @option_audit_probe = NEW.`id`",
		).Error)

		require.NoError(t, migrateOptionPrimaryKey(db))

		var owner string
		require.NoError(t, db.Raw(`
SELECT event_object_table
FROM information_schema.triggers
WHERE trigger_schema = DATABASE() AND trigger_name = ?`, trigger).Scan(&owner).Error)
		assert.Equal(t, "option_audit", owner)
	})

	t.Run("valid generated unknown trigger fails closed", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(
			"CREATE TABLE options (`key` varchar(191), `value` longtext) ENGINE=InnoDB",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
		).Error)
		require.NoError(t, db.Exec(
			"CREATE TABLE option_audit (`id` bigint PRIMARY KEY) ENGINE=InnoDB",
		).Error)
		trigger := optionInsertTriggerPrefix + strings.Repeat("b", optionArtifactIDSize*2)
		require.NoError(t, db.Exec(
			"CREATE TRIGGER "+quoteMySQLIdent(trigger)+" AFTER INSERT ON option_audit "+
				"FOR EACH ROW SET @option_audit_probe = NEW.`id`",
		).Error)
		before, err := showCreateMySQLTable(db, "options")
		require.NoError(t, err)
		recorder := &migrationSQLRecorder{}

		err = migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

		require.ErrorContains(t, err, "options migration artifact collision")
		assert.Empty(t, recorder.schemaMutations())
		after, showErr := showCreateMySQLTable(db, "options")
		require.NoError(t, showErr)
		assert.Equal(t, before, after)
	})
}

func TestOptionPrimaryKeyMigrationRejectsTransactionalTempTableCollision(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" {
		t.Skip("SQLite/PostgreSQL transactional artifact ownership regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (key varchar(191), value text, UNIQUE (key))",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (key, value) VALUES (?, ?)",
		"source.option", "source-preserved",
	).Error)
	require.NoError(t, db.Exec(
		"CREATE TABLE options_pk_tmp (id bigint PRIMARY KEY, payload text NOT NULL)",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options_pk_tmp (id, payload) VALUES (?, ?)",
		1, "business-preserved",
	).Error)
	recorder := &migrationSQLRecorder{}

	err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

	assert.ErrorContains(t, err, "options migration artifact collision")
	if err != nil {
		assert.NotContains(t, err.Error(), "business-preserved")
		assert.NotContains(t, err.Error(), "source-preserved")
	}
	assert.Empty(t, recorder.schemaMutations(), "collision must fail before temporary DDL")
	if assert.True(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable)) {
		var businessValue string
		require.NoError(t, db.Table(optionPrimaryKeyTmpTable).Select("payload").
			Where("id = ?", 1).Scan(&businessValue).Error)
		assert.Equal(t, "business-preserved", businessValue)
	}
	var sourceValue string
	require.NoError(t, db.Table("options").Select("value").
		Where("key = ?", "source.option").Scan(&sourceValue).Error)
	assert.Equal(t, "source-preserved", sourceValue)
	primary, primaryErr := optionsKeyIsPrimary(db)
	require.NoError(t, primaryErr)
	assert.False(t, primary, "failed migration must retain UNIQUE(key) without promoting it")
}

func TestOptionPrimaryKeyMigrationSkipsUnmarkedDynamicMySQLCrashOrphan(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 crash-before-marker regression")
	}
	db := useMigrationTestDB(t)
	orphanTable := optionMySQLTmpPrefix + strings.Repeat("a", optionArtifactIDSize*2)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (`key` varchar(191), `value` longtext) ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
	).Error)
	// Simulate a previous process dying after CREATE TABLE LIKE committed but
	// before it could add the ownership marker.
	require.NoError(t, db.Exec(
		"CREATE TABLE "+quoteMySQLIdent(orphanTable)+" LIKE `options`",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO "+quoteMySQLIdent(orphanTable)+" (`key`, `value`) VALUES ('orphan.option', 'untouched')",
	).Error)

	require.NoError(t, migrateOptionPrimaryKey(db))

	var sourceValue string
	require.NoError(t, db.Table("options").Select("value").
		Where("`key` = ?", "source.option").Scan(&sourceValue).Error)
	assert.Equal(t, "preserved", sourceValue)
	primary, err := optionsKeyIsPrimary(db)
	require.NoError(t, err)
	assert.True(t, primary)
	assert.True(t, db.Migrator().HasTable(orphanTable))
	var orphanValue string
	require.NoError(t, db.Table(orphanTable).Select("value").
		Where("`key` = ?", "orphan.option").Scan(&orphanValue).Error)
	assert.Equal(t, "untouched", orphanValue)
}

func TestOptionPrimaryKeyMigrationCreatesOwnedMySQLTempAtomically(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 atomic temporary-table ownership regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options ("+
			"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin,"+
			"`value` longtext,"+
			"`schema_marker` varchar(32) NOT NULL DEFAULT 'preserved',"+
			"KEY `idx_options_value` (`value`(16))"+
			") ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci "+
			"COMMENT='schema; preserved'",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
	).Error)

	recorder := &migrationSQLRecorder{}
	barrier := &optionMigrationSnapshotBarrier{
		Interface:         recorder,
		statement:         "CREATE TABLE `" + optionMySQLTmpPrefix,
		reached:           make(chan struct{}),
		continueMigration: make(chan struct{}),
	}
	migrationDone := make(chan error, 1)
	go func() {
		migrationDone <- migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: barrier}))
	}()
	select {
	case <-barrier.reached:
	case err := <-migrationDone:
		t.Fatalf("migration failed before atomic temporary-table creation: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("migration did not create the temporary table")
	}

	var temporaryTable string
	temporaryTableErr := db.Raw(`
SELECT table_name
FROM information_schema.tables
WHERE table_schema = DATABASE() AND table_name LIKE ?
ORDER BY table_name
LIMIT 1`, optionMySQLTmpPrefix+"%").Scan(&temporaryTable).Error
	var markerCount int64
	markerErr := db.Raw(`
SELECT count(*)
FROM information_schema.columns
WHERE table_schema = DATABASE()
  AND table_name = ?
  AND column_name = ?
  AND column_comment LIKE ?`,
		temporaryTable, optionMigrationMarkerCol, optionMigrationMarker+"/%",
	).Scan(&markerCount).Error
	var temporaryDDL string
	var temporaryDDLTable string
	showErr := db.Raw("SHOW CREATE TABLE "+quoteMySQLIdent(temporaryTable)).
		Row().Scan(&temporaryDDLTable, &temporaryDDL)
	close(barrier.continueMigration)
	migrationErr := <-migrationDone

	require.NoError(t, temporaryTableErr)
	require.NotEmpty(t, temporaryTable)
	require.NoError(t, markerErr)
	assert.EqualValues(t, 1, markerCount, "ownership marker must exist when CREATE returns")
	require.NoError(t, showErr)
	assert.Equal(t, temporaryTable, temporaryDDLTable)
	assert.Contains(t, temporaryDDL, "`schema_marker` varchar(32)")
	assert.Contains(t, temporaryDDL, "DEFAULT 'preserved'")
	assert.Contains(t, temporaryDDL, "KEY `idx_options_value` (`value`(16))")
	assert.Contains(t, temporaryDDL, "COLLATE=utf8mb4_unicode_ci")
	assert.Contains(t, temporaryDDL, "COMMENT='schema; preserved'")
	require.NoError(t, migrationErr)

	recorder.mu.Lock()
	statements := slices.Clone(recorder.statements)
	recorder.mu.Unlock()
	for _, statement := range statements {
		assert.NotContains(t, strings.ToUpper(statement), "SHOW CREATE TABLE")
	}
	artifacts := mysqlOptionMigrationArtifacts{
		ID:       strings.Repeat("ab", optionArtifactIDSize),
		Marker:   optionMigrationMarker + "/" + strings.Repeat("ab", optionArtifactIDSize),
		TmpTable: optionMySQLTmpPrefix + strings.Repeat("ab", optionArtifactIDSize),
	}
	for _, malformed := range []string{
		"CREATE TABLE options (`key` varchar(191)) ENGINE=InnoDB",
		"CREATE TABLE `other` (`key` varchar(191)) ENGINE=InnoDB",
		"CREATE TABLE `options` (`key` varchar(191) ENGINE=InnoDB",
		"CREATE TABLE `options` () ENGINE=InnoDB",
		"CREATE TABLE `options` (`key` varchar(191))",
		"CREATE TABLE `options` (`key` varchar(191)) ENGINE=InnoDB; DROP TABLE options",
	} {
		_, parsed := buildOwnedMySQLOptionCreateDDL(malformed, artifacts)
		assert.False(t, parsed, malformed)
	}
}

func TestOptionPrimaryKeyMigrationRejectsExcessUnmarkedMySQLOrphans(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 unmarked orphan bound regression")
	}
	const maxUnmarkedTables = 32
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (`key` varchar(191), `value` longtext) ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
	).Error)
	for i := 0; i < maxUnmarkedTables+1; i++ {
		table := optionMySQLTmpPrefix + fmt.Sprintf("%032x", i)
		require.NoError(t, db.Exec(
			"CREATE TABLE "+quoteMySQLIdent(table)+" (`id` bigint PRIMARY KEY) ENGINE=InnoDB",
		).Error)
	}

	err := migrateOptionPrimaryKey(db)

	require.ErrorContains(t, err, "unmarked temporary table limit exceeded")
	var orphanCount int64
	require.NoError(t, db.Raw(`
SELECT count(*)
FROM information_schema.tables
WHERE table_schema = DATABASE() AND table_name LIKE ?`,
		optionMySQLTmpPrefix+"%",
	).Scan(&orphanCount).Error)
	assert.EqualValues(t, maxUnmarkedTables+1, orphanCount)
	var sourceValue string
	require.NoError(t, db.Table("options").Select("value").
		Where("`key` = ?", "source.option").Scan(&sourceValue).Error)
	assert.Equal(t, "preserved", sourceValue)
}

func TestOptionPrimaryKeyMigrationRetriesMySQLArtifactIDCollision(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 artifact ID retry regression")
	}
	t.Run("uses next available ID", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(
			"CREATE TABLE options (`key` varchar(191), `value` longtext) ENGINE=InnoDB",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
		).Error)
		collidingID := strings.Repeat("11", optionArtifactIDSize)
		orphanTable := optionMySQLTmpPrefix + collidingID
		require.NoError(t, db.Exec(
			"CREATE TABLE "+quoteMySQLIdent(orphanTable)+" (`id` bigint PRIMARY KEY) ENGINE=InnoDB",
		).Error)

		var entropy bytes.Buffer
		entropy.Write(bytes.Repeat([]byte{0x01}, optionDigestKeySize))
		entropy.Write(bytes.Repeat([]byte{0x11}, optionArtifactIDSize))
		entropy.Write(bytes.Repeat([]byte{0x22}, optionArtifactIDSize))

		require.NoError(t, repairOptionPrimaryKey(db, &entropy))
		assert.True(t, db.Migrator().HasTable(orphanTable))
		var sourceValue string
		require.NoError(t, db.Table("options").Select("value").
			Where("`key` = ?", "source.option").Scan(&sourceValue).Error)
		assert.Equal(t, "preserved", sourceValue)
		primary, err := optionsKeyIsPrimary(db)
		require.NoError(t, err)
		assert.True(t, primary)
	})

	t.Run("retained backup ID uses next available ID", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(
			"CREATE TABLE options (`key` varchar(191), `value` longtext) ENGINE=InnoDB",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
		).Error)
		collidingID := strings.Repeat("33", optionArtifactIDSize)
		unownedBackup := optionLegacyTablePrefix + collidingID
		require.NoError(t, db.Exec(
			"CREATE TABLE "+quoteMySQLIdent(unownedBackup)+" (`id` bigint PRIMARY KEY) ENGINE=InnoDB",
		).Error)

		var entropy bytes.Buffer
		entropy.Write(bytes.Repeat([]byte{0x01}, optionDigestKeySize))
		entropy.Write(bytes.Repeat([]byte{0x33}, optionArtifactIDSize))
		entropy.Write(bytes.Repeat([]byte{0x44}, optionArtifactIDSize))

		require.NoError(t, repairOptionPrimaryKey(db, &entropy))
		assert.True(t, db.Migrator().HasTable(unownedBackup), "an unowned retained backup must not be replaced")
		primary, err := optionsKeyIsPrimary(db)
		require.NoError(t, err)
		assert.True(t, primary)
	})

	t.Run("stops after eight collisions", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(
			"CREATE TABLE options (`key` varchar(191), `value` longtext) ENGINE=InnoDB",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
		).Error)
		var entropy bytes.Buffer
		entropy.Write(bytes.Repeat([]byte{0x01}, optionDigestKeySize))
		for id := byte(1); id <= optionArtifactIDAttempts+1; id++ {
			encodedID := strings.Repeat(fmt.Sprintf("%02x", id), optionArtifactIDSize)
			if id <= optionArtifactIDAttempts {
				require.NoError(t, db.Exec(
					"CREATE TABLE "+quoteMySQLIdent(optionMySQLTmpPrefix+encodedID)+
						" (`id` bigint PRIMARY KEY) ENGINE=InnoDB",
				).Error)
			}
			entropy.Write(bytes.Repeat([]byte{id}, optionArtifactIDSize))
		}

		err := repairOptionPrimaryKey(db, &entropy)

		require.ErrorContains(t, err, "no unused id after 8 attempts")
		assert.Equal(t, optionArtifactIDSize, entropy.Len())
		primary, primaryErr := optionsKeyIsPrimary(db)
		require.NoError(t, primaryErr)
		assert.False(t, primary)
	})
}

func TestOptionPrimaryKeyMigrationMarksOwnedMySQLTriggers(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 trigger ownership regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (`key` varchar(191), `value` longtext) ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
	).Error)

	barrier := &optionMigrationSnapshotBarrier{
		Interface:         logger.Default.LogMode(logger.Silent),
		statement:         "CREATE TRIGGER `" + optionInsertTriggerPrefix,
		reached:           make(chan struct{}),
		continueMigration: make(chan struct{}),
	}
	migrationDone := make(chan error, 1)
	go func() {
		migrationDone <- migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: barrier}))
	}()
	select {
	case <-barrier.reached:
	case err := <-migrationDone:
		t.Fatalf("migration failed before installing all ownership-marked triggers: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("migration did not install all ownership-marked triggers")
	}

	var triggers []struct {
		Name      string `gorm:"column:TRIGGER_NAME"`
		Event     string `gorm:"column:EVENT_MANIPULATION"`
		Timing    string `gorm:"column:ACTION_TIMING"`
		TableName string `gorm:"column:EVENT_OBJECT_TABLE"`
		Action    string `gorm:"column:ACTION_STATEMENT"`
	}
	require.NoError(t, db.Raw(`
SELECT TRIGGER_NAME, EVENT_MANIPULATION, ACTION_TIMING, EVENT_OBJECT_TABLE, ACTION_STATEMENT
FROM information_schema.triggers
WHERE trigger_schema = DATABASE() AND action_statement LIKE ?
ORDER BY trigger_name`, "%"+optionMigrationMarker+"/%").Scan(&triggers).Error)
	require.Len(t, triggers, 3)
	for _, trigger := range triggers {
		assert.Equal(t, "AFTER", trigger.Timing)
		assert.Equal(t, "options", trigger.TableName)
		assert.Equal(t, 2, strings.Count(trigger.Action, optionMigrationMarker+"/"))
	}
	close(barrier.continueMigration)
	require.NoError(t, <-migrationDone)
	assertNoMySQLOptionMigrationArtifacts(t, db)
}

func TestOptionPrimaryKeyMigrationRecoversOwnedMySQLCrashArtifacts(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 crash artifact recovery")
	}
	for _, test := range []struct {
		name        string
		dynamic     bool
		afterRename bool
	}{
		{name: "legacy before rename"},
		{name: "legacy after rename", afterRename: true},
		{name: "dynamic before rename", dynamic: true},
		{name: "dynamic after rename", dynamic: true, afterRename: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options (`key` varchar(191), `value` longtext) ENGINE=InnoDB",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
			).Error)
			columns, err := mysqlWritableOptionColumns(db)
			require.NoError(t, err)
			artifacts := legacyMySQLOptionMigrationArtifacts()
			if test.dynamic {
				artifacts, err = newMySQLOptionMigrationArtifacts(cryptorand.Reader)
				require.NoError(t, err)
			}
			require.NoError(t, db.Exec(
				"CREATE TABLE "+quoteMySQLIdent(artifacts.TmpTable)+" LIKE `options`",
			).Error)
			require.NoError(t, addMySQLOptionMigrationMarker(db, artifacts.TmpTable, artifacts.Marker))
			require.NoError(t, db.Exec(
				"ALTER TABLE "+quoteMySQLIdent(artifacts.TmpTable)+" ADD PRIMARY KEY (`key`)",
			).Error)
			require.NoError(t, createMySQLOptionMigrationTriggers(db, columns, artifacts))
			require.NoError(t, db.Exec(
				"INSERT INTO "+quoteMySQLIdent(artifacts.TmpTable)+
					" (`key`, `value`) SELECT `key`, `value` FROM options",
			).Error)
			backup := "options_legacy_123456789"
			if test.afterRename {
				require.NoError(t, swapOptionTables(db, artifacts.TmpTable, backup))
			}

			migrationErr := migrateOptionPrimaryKey(db)
			if test.afterRename {
				require.ErrorContains(t, migrationErr, "legacy active replacement")
				assert.True(t, db.Migrator().HasTable("options"))
				assert.True(t, db.Migrator().HasTable(backup))
				primary, err := optionsKeyIsPrimary(db)
				require.NoError(t, err)
				assert.True(t, primary, "ambiguous active replacement must remain active")
				assert.True(t, db.Migrator().HasColumn("options", optionMigrationMarkerCol))
				var triggerCount int64
				require.NoError(t, db.Raw(`
SELECT count(*)
FROM information_schema.triggers
WHERE trigger_schema = DATABASE() AND event_object_table = ?`,
					backup).Scan(&triggerCount).Error)
				assert.EqualValues(t, 3, triggerCount)
				require.ErrorContains(t,
					migrateOptionPrimaryKey(db.Session(&gorm.Session{NewDB: true})),
					"legacy active replacement",
				)
				return
			}
			require.NoError(t, migrationErr)
			assertNoMySQLOptionMigrationArtifacts(t, db)
			var sourceValue string
			require.NoError(t, db.Table("options").Select("value").
				Where("`key` = ?", "source.option").Scan(&sourceValue).Error)
			assert.Equal(t, "preserved", sourceValue)
		})
	}
}

func TestOptionPrimaryKeyMigrationRecoversPostSwapMySQLCrash(t *testing.T) {
	const (
		helperEnv      = "OPTION_MIGRATION_POST_SWAP_CRASH_HELPER"
		helperDSNEnv   = "OPTION_MIGRATION_POST_SWAP_CRASH_DSN"
		helperReadyEnv = "OPTION_MIGRATION_POST_SWAP_CRASH_READY"
		helperGoEnv    = "OPTION_MIGRATION_POST_SWAP_CRASH_GO"
	)
	if os.Getenv(helperEnv) == "1" {
		crashLogger := &optionMigrationCrashLogger{
			Interface:    logger.Default.LogMode(logger.Silent),
			readyPath:    os.Getenv(helperReadyEnv),
			continuePath: os.Getenv(helperGoEnv),
		}
		db, err := gorm.Open(gormMySQL.Open(os.Getenv(helperDSNEnv)), &gorm.Config{Logger: crashLogger})
		if err != nil {
			os.Exit(88)
		}
		common.SetMainDatabaseType(common.DatabaseTypeMySQL)
		if migrateOptionPrimaryKey(db) != nil {
			os.Exit(89)
		}
		os.Exit(90)
	}
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 post-swap crash recovery")
	}

	for round := range 3 {
		for _, test := range []struct {
			name                      string
			mutateRows                bool
			firstRestartMayCheckpoint bool
			drift                     func(*testing.T, *gorm.DB, string)
			verify                    func(*testing.T, *gorm.DB, string)
			wantError                 string
		}{
			{name: "no drift"},
			{
				name:                      "active replacement row drift",
				mutateRows:                true,
				firstRestartMayCheckpoint: true,
				wantError:                 "options migration recovery blocked",
			},
			{
				name: "retained source schema drift",
				drift: func(t *testing.T, db *gorm.DB, backup string) {
					require.NoError(t, db.Exec(
						"ALTER TABLE "+quoteMySQLIdent(backup)+" ADD COLUMN source_drift "+
							"varchar(32) NOT NULL DEFAULT 'source-preserved'",
					).Error)
				},
				wantError: "MySQL options schema changed during migration",
			},
			{
				name: "active replacement extra column",
				drift: func(t *testing.T, db *gorm.DB, _ string) {
					require.NoError(t, db.Exec(
						"ALTER TABLE options ADD COLUMN active_drift varchar(32)",
					).Error)
				},
				wantError: "options migration artifact collision",
			},
			{
				name: "active replacement missing column",
				drift: func(t *testing.T, db *gorm.DB, _ string) {
					require.NoError(t, db.Exec(
						"ALTER TABLE options DROP COLUMN schema_marker",
					).Error)
				},
				wantError: "options migration artifact collision",
			},
			{
				name: "active replacement extra index",
				drift: func(t *testing.T, db *gorm.DB, _ string) {
					require.NoError(t, db.Exec(
						"ALTER TABLE options ADD INDEX idx_active_drift (`key`, `value`(16))",
					).Error)
				},
				wantError: "MySQL options schema changed during migration",
			},
			{
				name: "active replacement missing index",
				drift: func(t *testing.T, db *gorm.DB, _ string) {
					require.NoError(t, db.Exec(
						"ALTER TABLE options DROP INDEX idx_options_value",
					).Error)
				},
				wantError: "MySQL options schema changed during migration",
			},
			{
				name: "active replacement collation drift",
				drift: func(t *testing.T, db *gorm.DB, _ string) {
					require.NoError(t, db.Exec(
						"ALTER TABLE options DEFAULT CHARACTER SET latin1 COLLATE latin1_bin",
					).Error)
				},
				wantError: "MySQL options schema changed during migration",
			},
			{
				name: "active replacement engine drift",
				drift: func(t *testing.T, db *gorm.DB, _ string) {
					require.NoError(t, db.Exec("ALTER TABLE options ENGINE=MyISAM").Error)
				},
				wantError: "MySQL options schema changed during migration",
			},
			{
				name: "active replacement owner marker drift",
				drift: func(t *testing.T, db *gorm.DB, _ string) {
					require.NoError(t, db.Exec(
						"ALTER TABLE options MODIFY COLUMN "+quoteMySQLIdent(optionMigrationMarkerCol)+
							" TINYINT UNSIGNED NOT NULL DEFAULT 1 COMMENT 'unknown-owner'",
					).Error)
				},
				wantError: "options migration artifact collision",
			},
			{
				name: "active replacement phase drift",
				drift: func(t *testing.T, db *gorm.DB, _ string) {
					require.NoError(t, db.Exec(
						"ALTER TABLE options ALTER COLUMN "+quoteMySQLIdent(optionMigrationStateCol)+
							" SET DEFAULT 'invalid-phase'",
					).Error)
				},
				wantError: "options migration artifact collision",
			},
			{
				name: "active replacement primary key drift",
				drift: func(t *testing.T, db *gorm.DB, _ string) {
					require.NoError(t, db.Exec("ALTER TABLE options DROP PRIMARY KEY").Error)
				},
				wantError: "MySQL options schema changed during migration",
			},
			{
				name: "active replacement custom trigger",
				drift: func(t *testing.T, db *gorm.DB, _ string) {
					require.NoError(t, db.Exec(
						"CREATE TRIGGER options_recovery_custom_bi BEFORE INSERT ON options "+
							"FOR EACH ROW SET NEW.`value` = CONCAT(NEW.`value`, '-custom')",
					).Error)
				},
				verify: func(t *testing.T, db *gorm.DB, _ string) {
					var trigger mysqlOptionTrigger
					require.NoError(t, db.Raw(`
SELECT TRIGGER_NAME AS trigger_name,
       EVENT_MANIPULATION AS event_manipulation,
       ACTION_TIMING AS action_timing,
       EVENT_OBJECT_TABLE AS event_object_table,
       ACTION_STATEMENT AS action_statement
FROM information_schema.triggers
WHERE trigger_schema = DATABASE() AND trigger_name = 'options_recovery_custom_bi'`,
					).Scan(&trigger).Error)
					assert.Equal(t, "options_recovery_custom_bi", trigger.Name)
					assert.Equal(t, "INSERT", trigger.Event)
					assert.Equal(t, "BEFORE", trigger.Timing)
					assert.Equal(t, "options", trigger.TableName)
					assert.Equal(t,
						normalizeMySQLTriggerAction(
							"SET NEW.`value` = CONCAT(NEW.`value`, '-custom')",
						),
						normalizeMySQLTriggerAction(trigger.Action),
					)
				},
				wantError: "MySQL options schema changed during migration",
			},
			{
				name: "active replacement incoming foreign key",
				drift: func(t *testing.T, db *gorm.DB, _ string) {
					require.NoError(t, db.Exec(
						"CREATE TABLE options_recovery_reference ("+
							"`id` bigint PRIMARY KEY, "+
							"`option_key` varchar(191) CHARACTER SET utf8mb4 "+
							"COLLATE utf8mb4_unicode_ci NOT NULL, "+
							"CONSTRAINT fk_options_recovery_reference "+
							"FOREIGN KEY (`option_key`) REFERENCES options (`key`)"+
							") ENGINE=InnoDB",
					).Error)
					require.NoError(t, db.Exec(
						"INSERT INTO options_recovery_reference (`id`, `option_key`) VALUES (1, ?)",
						"keep.option",
					).Error)
				},
				verify: func(t *testing.T, db *gorm.DB, _ string) {
					var foreignKey struct {
						TableName            string `gorm:"column:table_name"`
						ColumnName           string `gorm:"column:column_name"`
						ReferencedTableName  string `gorm:"column:referenced_table_name"`
						ReferencedColumnName string `gorm:"column:referenced_column_name"`
					}
					require.NoError(t, db.Raw(`
SELECT table_name, column_name, referenced_table_name, referenced_column_name
FROM information_schema.key_column_usage
WHERE constraint_schema = DATABASE()
  AND constraint_name = 'fk_options_recovery_reference'`,
					).Scan(&foreignKey).Error)
					assert.Equal(t, "options_recovery_reference", foreignKey.TableName)
					assert.Equal(t, "option_key", foreignKey.ColumnName)
					assert.Equal(t, "options", foreignKey.ReferencedTableName)
					assert.Equal(t, "key", foreignKey.ReferencedColumnName)
					var optionKey string
					require.NoError(t, db.Table("options_recovery_reference").
						Select("option_key").Where("id = 1").Scan(&optionKey).Error)
					assert.Equal(t, "keep.option", optionKey)
				},
				wantError: "MySQL options schema changed during migration",
			},
		} {
			t.Run(fmt.Sprintf("round_%d/%s", round+1, test.name), func(t *testing.T) {
				db := useMigrationTestDB(t)
				require.NoError(t, db.Exec(
					"CREATE TABLE options ("+
						"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
						"`value` longtext,"+
						"`schema_marker` varchar(32) NOT NULL DEFAULT 'source-default',"+
						"KEY `idx_options_value` (`value`(16))"+
						") ENGINE=InnoDB",
				).Error)
				require.NoError(t, db.Exec(
					"INSERT INTO options (`key`, `value`) VALUES (?, ?), (?, ?), (?, ?)",
					"delete.option", "delete-before",
					"keep.option", "keep",
					"update.option", "update-before",
				).Error)
				expectedRows := []Option{
					{Key: "delete.option", Value: "delete-before"},
					{Key: "keep.option", Value: "keep"},
					{Key: "update.option", Value: "update-before"},
				}

				var databaseName string
				require.NoError(t, db.Raw("SELECT DATABASE()").Scan(&databaseName).Error)
				parsed, err := mysqlDriver.ParseDSN(os.Getenv("APP_PLUGIN_TEST_DSN"))
				require.NoError(t, err)
				parsed.DBName = databaseName
				coordination := t.TempDir()
				readyPath := filepath.Join(coordination, "unlocked")
				continuePath := filepath.Join(coordination, "continue")
				executable, err := os.Executable()
				require.NoError(t, err)
				command := exec.CommandContext(
					t.Context(),
					executable,
					"-test.run=^TestOptionPrimaryKeyMigrationRecoversPostSwapMySQLCrash$",
					"-test.v",
				)
				command.Env = append(os.Environ(),
					helperEnv+"=1",
					helperDSNEnv+"="+parsed.FormatDSN(),
					helperReadyEnv+"="+readyPath,
					helperGoEnv+"="+continuePath,
				)
				require.NoError(t, command.Start())
				require.Eventually(t, func() bool {
					_, statErr := os.Stat(readyPath)
					return statErr == nil
				}, 10*time.Second, 10*time.Millisecond, "migration did not expose the post-unlock window")

				require.NoError(t, os.WriteFile(continuePath, []byte("continue"), 0o600))
				waitErr := command.Wait()
				var exitErr *exec.ExitError
				require.ErrorAs(t, waitErr, &exitErr)
				require.Equal(t, 86, exitErr.ExitCode(), "crash helper must stop immediately after the atomic swap")

				if test.mutateRows {
					require.NoError(t, db.Exec(
						"UPDATE options SET `value` = ? WHERE `key` = ?",
						"update-after", "update.option",
					).Error)
					require.NoError(t, db.Exec(
						"DELETE FROM options WHERE `key` = ?",
						"delete.option",
					).Error)
					require.NoError(t, db.Exec(
						"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
						"insert.option", "insert-after",
					).Error)
					expectedRows = []Option{
						{Key: "insert.option", Value: "insert-after"},
						{Key: "keep.option", Value: "keep"},
						{Key: "update.option", Value: "update-after"},
					}
				}

				primary, err := optionsKeyIsPrimary(db)
				require.NoError(t, err)
				require.True(t, primary, "the crash must leave the owned replacement active")
				var marker string
				require.NoError(t, db.Raw(`
SELECT column_comment
FROM information_schema.columns
WHERE table_schema = DATABASE() AND table_name = 'options' AND column_name = ?`,
					optionMigrationMarkerCol).Scan(&marker).Error)
				artifacts, ok := parseMySQLOptionMigrationArtifacts("options", marker)
				require.True(t, ok)
				backup := optionLegacyTablePrefix + artifacts.ID
				require.True(t, db.Migrator().HasTable(backup))
				state, hasState, err := inspectMySQLOptionMigrationState(db, "options")
				require.NoError(t, err)
				require.True(t, hasState)
				assert.Equal(t, optionMigrationPrepared, state.Phase)
				assert.Len(t, state.SourceSchemaSHA256, sha256.Size*2)
				assert.Len(t, state.ActiveSchemaSHA256, sha256.Size*2)

				if test.drift != nil {
					test.drift(t, db, backup)
				}
				if test.verify != nil {
					test.verify(t, db, backup)
				}
				beforeRestart := captureMySQLOptionRecoverySnapshot(t, db, backup)

				restartErr := migrateOptionPrimaryKey(db.Session(&gorm.Session{NewDB: true}))
				if test.wantError != "" {
					require.ErrorContains(t, restartErr, test.wantError)
					assert.True(t, db.Migrator().HasTable("options"))
					assert.True(t, db.Migrator().HasTable(backup))
					assert.True(t, db.Migrator().HasColumn("options", optionMigrationMarkerCol))
					assert.True(t, db.Migrator().HasColumn("options", optionMigrationStateCol))
					afterRestart := captureMySQLOptionRecoverySnapshot(t, db, backup)
					if test.firstRestartMayCheckpoint {
						assert.Equal(t, beforeRestart.ActiveRows, afterRestart.ActiveRows)
						assert.Equal(t, beforeRestart.BackupRows, afterRestart.BackupRows)
					} else {
						assert.Equal(t, beforeRestart, afterRestart)
					}
					if test.verify != nil {
						test.verify(t, db, backup)
					}

					secondErr := migrateOptionPrimaryKey(db.Session(&gorm.Session{NewDB: true}))
					require.ErrorContains(t, secondErr, test.wantError)
					afterSecondRestart := captureMySQLOptionRecoverySnapshot(t, db, backup)
					assert.Equal(t, afterRestart, afterSecondRestart)
					if test.verify != nil {
						test.verify(t, db, backup)
					}
				} else {
					require.NoError(t, restartErr)
					primary, err = optionsKeyIsPrimary(db)
					require.NoError(t, err)
					assert.True(t, primary)
					assertNoMySQLOptionMigrationArtifacts(t, db)
					require.NoError(t, migrateOptionPrimaryKey(db.Session(&gorm.Session{NewDB: true})))
					assertNoMySQLOptionMigrationArtifacts(t, db)
				}

				var rows []Option
				require.NoError(t, db.Table("options").Order("`key`").Find(&rows).Error)
				assert.Equal(t, expectedRows, rows,
					"restart must preserve committed post-swap active-table state")
			})
		}
	}
}

func TestOptionPrimaryKeyMigrationRejectsMySQLCatalogChangesAtFinalization(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 finalization catalog regression")
	}
	type catalogMutation struct {
		name    string
		apply   func(*testing.T, *gorm.DB) string
		capture func(*testing.T, *gorm.DB) string
	}
	mutations := []catalogMutation{
		{
			name: "active trigger",
			apply: func(t *testing.T, db *gorm.DB) string {
				t.Helper()
				require.NoError(t, db.Exec(
					"CREATE TRIGGER options_finalize_custom_bi BEFORE INSERT ON options "+
						"FOR EACH ROW SET NEW.`value` = CONCAT(NEW.`value`, '-custom')",
				).Error)
				var trigger mysqlOptionTrigger
				require.NoError(t, db.Raw(`
SELECT TRIGGER_NAME AS trigger_name,
       EVENT_MANIPULATION AS event_manipulation,
       ACTION_TIMING AS action_timing,
       EVENT_OBJECT_TABLE AS event_object_table,
       ACTION_STATEMENT AS action_statement
FROM information_schema.triggers
WHERE trigger_schema = DATABASE() AND trigger_name = 'options_finalize_custom_bi'`,
				).Scan(&trigger).Error)
				require.Equal(t, "options_finalize_custom_bi", trigger.Name)
				return strings.Join([]string{
					trigger.Name, trigger.Event, trigger.Timing, trigger.TableName,
					normalizeMySQLTriggerAction(trigger.Action),
				}, "|")
			},
			capture: func(t *testing.T, db *gorm.DB) string {
				t.Helper()
				var trigger mysqlOptionTrigger
				require.NoError(t, db.Raw(`
SELECT TRIGGER_NAME AS trigger_name,
       EVENT_MANIPULATION AS event_manipulation,
       ACTION_TIMING AS action_timing,
       EVENT_OBJECT_TABLE AS event_object_table,
       ACTION_STATEMENT AS action_statement
FROM information_schema.triggers
WHERE trigger_schema = DATABASE() AND trigger_name = 'options_finalize_custom_bi'`,
				).Scan(&trigger).Error)
				return strings.Join([]string{
					trigger.Name, trigger.Event, trigger.Timing, trigger.TableName,
					normalizeMySQLTriggerAction(trigger.Action),
				}, "|")
			},
		},
		{
			name: "incoming foreign key",
			apply: func(t *testing.T, db *gorm.DB) string {
				t.Helper()
				require.NoError(t, db.Exec(
					"CREATE TABLE options_finalize_reference ("+
						"`id` bigint PRIMARY KEY, "+
						"`option_key` varchar(191) CHARACTER SET utf8mb4 "+
						"COLLATE utf8mb4_unicode_ci NOT NULL, "+
						"CONSTRAINT fk_options_finalize_reference "+
						"FOREIGN KEY (`option_key`) REFERENCES options (`key`)"+
						") ENGINE=InnoDB",
				).Error)
				require.NoError(t, db.Exec(
					"INSERT INTO options_finalize_reference (`id`, `option_key`) VALUES (1, ?)",
					"keep.option",
				).Error)
				ddl, err := showCreateMySQLTable(db, "options_finalize_reference")
				require.NoError(t, err)
				return ddl
			},
			capture: func(t *testing.T, db *gorm.DB) string {
				t.Helper()
				ddl, err := showCreateMySQLTable(db, "options_finalize_reference")
				require.NoError(t, err)
				var optionKey string
				require.NoError(t, db.Table("options_finalize_reference").
					Select("option_key").Where("id = 1").Scan(&optionKey).Error)
				require.Equal(t, "keep.option", optionKey)
				return ddl
			},
		},
		{
			name: "cross-schema incoming foreign key",
			apply: func(t *testing.T, db *gorm.DB) string {
				t.Helper()
				return createMySQLCrossSchemaOptionReference(t, db, "finalize", "options")
			},
			capture: func(t *testing.T, db *gorm.DB) string {
				t.Helper()
				return captureMySQLCrossSchemaOptionReference(t, db, "finalize")
			},
		},
	}
	for _, recovery := range []bool{false, true} {
		path := "ordinary migration"
		if recovery {
			path = "post-swap recovery"
		}
		for _, mutation := range mutations {
			t.Run(path+"/"+mutation.name, func(t *testing.T) {
				db := useMigrationTestDB(t)
				require.NoError(t, db.Exec(
					"CREATE TABLE options ("+
						"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
						"`value` longtext,"+
						"`schema_marker` varchar(32) NOT NULL DEFAULT 'source-default',"+
						"KEY `idx_options_value` (`value`(16))"+
						") ENGINE=InnoDB",
				).Error)
				require.NoError(t, db.Exec(
					"INSERT INTO options (`key`, `value`) VALUES (?, ?), (?, ?)",
					"keep.option", "keep",
					"second.option", "second",
				).Error)

				if recovery {
					crashMySQLOptionMigrationAfterSwap(t, db)
				}
				barrier := &optionMigrationSnapshotBarrier{
					Interface:         logger.Default.LogMode(logger.Silent),
					statement:         "AS has_incoming_foreign_key",
					reached:           make(chan struct{}),
					continueMigration: make(chan struct{}),
				}
				if !recovery {
					barrier.statement = "constraint_name = 'PRIMARY'"
					barrier.additionalMatch = "table_name = 'options'"
				}
				migrationDone := make(chan error, 1)
				go func() {
					migrationDone <- migrateOptionPrimaryKey(
						db.Session(&gorm.Session{Logger: barrier, NewDB: true}),
					)
				}()
				select {
				case <-barrier.reached:
				case err := <-migrationDone:
					t.Fatalf("migration failed before the finalization barrier: %v", err)
				case <-time.After(10 * time.Second):
					t.Fatal("migration did not reach the finalization barrier")
				}

				externalDefinition := mutation.apply(t, db)
				var marker string
				require.NoError(t, db.Raw(`
SELECT column_comment
FROM information_schema.columns
WHERE table_schema = DATABASE() AND table_name = 'options' AND column_name = ?`,
					optionMigrationMarkerCol).Scan(&marker).Error)
				artifacts, ok := parseMySQLOptionMigrationArtifacts("options", marker)
				require.True(t, ok)
				backup := optionLegacyTablePrefix + artifacts.ID
				beforeFinalize := captureMySQLOptionRecoverySnapshot(t, db, backup)
				close(barrier.continueMigration)

				require.ErrorContains(t, <-migrationDone, "MySQL options schema changed during migration")
				afterFinalize := captureMySQLOptionRecoverySnapshot(t, db, backup)
				assert.Equal(t, beforeFinalize, afterFinalize)
				assert.Equal(t, externalDefinition, mutation.capture(t, db))
				assert.True(t, db.Migrator().HasColumn("options", optionMigrationMarkerCol))
				assert.True(t, db.Migrator().HasColumn("options", optionMigrationStateCol))

				require.ErrorContains(t,
					migrateOptionPrimaryKey(db.Session(&gorm.Session{NewDB: true})),
					"MySQL options schema changed during migration",
				)
				afterRestart := captureMySQLOptionRecoverySnapshot(t, db, backup)
				assert.Equal(t, beforeFinalize, afterRestart)
				assert.Equal(t, externalDefinition, mutation.capture(t, db))
			})
		}
	}

	for _, stage := range []struct {
		name          string
		statement     string
		expectedPhase string
		bridgePhase   string
		hasMarkers    bool
		restartFails  bool
	}{
		{
			name:          "after phase transition",
			statement:     "SET DEFAULT 'validated:",
			expectedPhase: optionMigrationValidated,
			bridgePhase:   optionMigrationValidated,
			hasMarkers:    true,
			restartFails:  true,
		},
		{
			name:          "after bridge trigger drop",
			statement:     "DROP TRIGGER `" + optionInsertTriggerPrefix,
			expectedPhase: "forward/i/drop_insert",
			bridgePhase:   optionMigrationInsertGone,
			hasMarkers:    true,
			restartFails:  true,
		},
		{
			name:         "after marker cleanup",
			statement:    "DROP COLUMN `" + optionMigrationStateCol + "`",
			bridgePhase:  optionMigrationBridgesGone,
			restartFails: true,
		},
	} {
		for _, mutation := range mutations[1:] {
			t.Run("post-swap recovery/"+mutation.name+"/"+stage.name, func(t *testing.T) {
				db := useMigrationTestDB(t)
				require.NoError(t, db.Exec(
					"CREATE TABLE options ("+
						"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
						"`value` longtext,"+
						"`schema_marker` varchar(32) NOT NULL DEFAULT 'source-default',"+
						"KEY `idx_options_value` (`value`(16))"+
						") ENGINE=InnoDB",
				).Error)
				require.NoError(t, db.Exec(
					"INSERT INTO options (`key`, `value`) VALUES (?, ?), (?, ?)",
					"keep.option", "keep",
					"second.option", "second",
				).Error)
				backup := crashMySQLOptionMigrationAfterSwap(t, db)

				barrier := &optionMigrationSnapshotBarrier{
					Interface:         logger.Default.LogMode(logger.Silent),
					statement:         stage.statement,
					reached:           make(chan struct{}),
					continueMigration: make(chan struct{}),
				}
				migrationDone := make(chan error, 1)
				go func() {
					migrationDone <- migrateOptionPrimaryKey(
						db.Session(&gorm.Session{Logger: barrier, NewDB: true}),
					)
				}()
				select {
				case <-barrier.reached:
				case err := <-migrationDone:
					t.Fatalf("migration failed before %s: %v", stage.name, err)
				case <-time.After(10 * time.Second):
					t.Fatalf("migration did not reach %s", stage.name)
				}

				var externalDefinition string
				func() {
					defer close(barrier.continueMigration)
					externalDefinition = mutation.apply(t, db)
				}()

				require.ErrorContains(t, <-migrationDone, "MySQL options schema changed during migration")
				afterFinalize := captureMySQLOptionRecoverySnapshot(t, db, backup)
				assert.Equal(t, externalDefinition, mutation.capture(t, db))
				assert.Equal(t, stage.hasMarkers,
					db.Migrator().HasColumn("options", optionMigrationMarkerCol))
				assert.Equal(t, stage.hasMarkers,
					db.Migrator().HasColumn("options", optionMigrationStateCol))
				state, hasState, err := inspectMySQLOptionMigrationState(db, "options")
				require.NoError(t, err)
				assert.Equal(t, stage.hasMarkers, hasState)
				if stage.hasMarkers {
					assert.Equal(t, stage.expectedPhase, state.Phase)
				}
				var marker string
				if stage.hasMarkers {
					require.NoError(t, db.Raw(`
SELECT column_comment
FROM information_schema.columns
WHERE table_schema = DATABASE() AND table_name = 'options' AND column_name = ?`,
						optionMigrationMarkerCol).Scan(&marker).Error)
					artifacts, ok := parseMySQLOptionMigrationArtifacts("options", marker)
					require.True(t, ok)
					expectedTriggers, validPhase := mysqlOptionMigrationBridgeNamesForPhase(
						artifacts, stage.bridgePhase,
					)
					require.True(t, validPhase)
					actualTriggers, err := inspectMySQLOptionMigrationTriggerCandidates(db, artifacts)
					require.NoError(t, err)
					actualNames := make([]string, len(actualTriggers))
					for i, trigger := range actualTriggers {
						actualNames[i] = trigger.Name
					}
					assert.ElementsMatch(t, expectedTriggers, actualNames)
				} else {
					assert.Empty(t, afterFinalize.Triggers)
				}

				restartErr := migrateOptionPrimaryKey(
					db.Session(&gorm.Session{NewDB: true}),
				)
				if stage.restartFails {
					require.ErrorContains(t, restartErr,
						"MySQL options schema changed during migration")
				} else {
					require.NoError(t, restartErr)
				}
				afterRestart := captureMySQLOptionRecoverySnapshot(t, db, backup)
				assert.Equal(t, afterFinalize, afterRestart)
				assert.Equal(t, externalDefinition, mutation.capture(t, db))
			})
		}
	}
}

func TestOptionPrimaryKeyMigrationRevalidatesFullStateAfterPhaseTransition(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 finalization full-state regression")
	}
	type phaseMutation struct {
		name            string
		restartRecovers bool
		apply           func(context.Context, gorm.ConnPool, string, mysqlOptionMigrationArtifacts) error
		verify          func(*testing.T, *gorm.DB, string, mysqlOptionMigrationArtifacts)
	}
	mutations := []phaseMutation{
		{
			name: "active primary key drop",
			apply: func(
				ctx context.Context,
				conn gorm.ConnPool,
				_ string,
				_ mysqlOptionMigrationArtifacts,
			) error {
				_, err := conn.ExecContext(ctx, "ALTER TABLE options DROP PRIMARY KEY")
				return err
			},
			verify: func(t *testing.T, db *gorm.DB, _ string, _ mysqlOptionMigrationArtifacts) {
				t.Helper()
				primary, err := mysqlOptionKeyIsSolePrimary(db, "options")
				require.NoError(t, err)
				assert.False(t, primary, "external active primary-key drift must be preserved")
			},
		},
		{
			name: "active extra column",
			apply: func(
				ctx context.Context,
				conn gorm.ConnPool,
				_ string,
				_ mysqlOptionMigrationArtifacts,
			) error {
				_, err := conn.ExecContext(ctx,
					"ALTER TABLE options ADD COLUMN full_stage_active_drift varchar(32)",
				)
				return err
			},
			verify: func(t *testing.T, db *gorm.DB, _ string, _ mysqlOptionMigrationArtifacts) {
				t.Helper()
				assert.True(t, db.Migrator().HasColumn("options", "full_stage_active_drift"))
			},
		},
		{
			name: "retained schema drift",
			apply: func(
				ctx context.Context,
				conn gorm.ConnPool,
				backup string,
				_ mysqlOptionMigrationArtifacts,
			) error {
				_, err := conn.ExecContext(ctx,
					"ALTER TABLE "+quoteMySQLIdent(backup)+
						" ADD COLUMN full_stage_retained_drift varchar(32)")
				return err
			},
			verify: func(t *testing.T, db *gorm.DB, backup string, _ mysqlOptionMigrationArtifacts) {
				t.Helper()
				assert.True(t, db.Migrator().HasColumn(backup, "full_stage_retained_drift"))
			},
		},
		{
			name: "owned bridge delete",
			apply: func(
				ctx context.Context,
				conn gorm.ConnPool,
				_ string,
				artifacts mysqlOptionMigrationArtifacts,
			) error {
				_, err := conn.ExecContext(
					ctx, "DROP TRIGGER "+quoteMySQLIdent(artifacts.InsertTrigger),
				)
				return err
			},
			verify: func(t *testing.T, db *gorm.DB, backup string, artifacts mysqlOptionMigrationArtifacts) {
				t.Helper()
				triggers, err := inspectMySQLOptionMigrationTriggerCandidates(db, artifacts)
				require.NoError(t, err)
				require.Len(t, triggers, 2, "an externally deleted bridge must not be restored")
				for _, trigger := range triggers {
					assert.Equal(t, backup, trigger.TableName)
					assert.NotEqual(t, artifacts.InsertTrigger, trigger.Name)
				}
			},
		},
		{
			name: "owned bridge body drift",
			apply: func(
				ctx context.Context,
				conn gorm.ConnPool,
				backup string,
				artifacts mysqlOptionMigrationArtifacts,
			) error {
				var original mysqlOptionTrigger
				if err := conn.QueryRowContext(ctx, `
SELECT TRIGGER_NAME, EVENT_MANIPULATION, ACTION_TIMING, EVENT_OBJECT_TABLE, ACTION_STATEMENT
FROM information_schema.triggers
WHERE trigger_schema = DATABASE() AND trigger_name = ?`,
					artifacts.InsertTrigger,
				).Scan(
					&original.Name, &original.Event, &original.Timing,
					&original.TableName, &original.Action,
				); err != nil {
					return err
				}
				if _, err := conn.ExecContext(
					ctx, "DROP TRIGGER "+quoteMySQLIdent(artifacts.InsertTrigger),
				); err != nil {
					return err
				}
				alteredBody, ok := strings.CutSuffix(strings.TrimSpace(original.Action), "END")
				if !ok {
					return fmt.Errorf("generated trigger body has no END suffix")
				}
				_, err := conn.ExecContext(ctx,
					"CREATE TRIGGER "+quoteMySQLIdent(artifacts.InsertTrigger)+" "+
						original.Timing+" "+original.Event+" ON "+quoteMySQLIdent(backup)+
						" FOR EACH ROW "+alteredBody+" SET @new_api_full_stage_drift = 1; END",
				)
				return err
			},
			verify: func(t *testing.T, db *gorm.DB, backup string, artifacts mysqlOptionMigrationArtifacts) {
				t.Helper()
				triggers, err := inspectMySQLOptionMigrationTriggerCandidates(db, artifacts)
				require.NoError(t, err)
				require.Len(t, triggers, 3, "a drifted bridge set must remain unchanged")
				var foundDrift bool
				for _, trigger := range triggers {
					assert.Equal(t, backup, trigger.TableName)
					if trigger.Name == artifacts.InsertTrigger {
						foundDrift = strings.Contains(
							trigger.Action, "@new_api_full_stage_drift",
						)
					}
				}
				assert.True(t, foundDrift, "the external bridge body drift must be preserved")
			},
		},
	}

	for _, recovery := range []bool{false, true} {
		path := "ordinary migration"
		if recovery {
			path = "post-swap recovery"
		}
		for _, mutation := range mutations {
			t.Run(path+"/"+mutation.name, func(t *testing.T) {
				db := useMigrationTestDB(t)
				require.NoError(t, db.Exec(
					"CREATE TABLE options ("+
						"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
						"`value` longtext,"+
						"`schema_marker` varchar(32) NOT NULL DEFAULT 'source-default',"+
						"KEY `idx_options_value` (`value`(16))"+
						") ENGINE=InnoDB",
				).Error)
				require.NoError(t, db.Exec(
					"INSERT INTO options (`key`, `value`) VALUES (?, ?), (?, ?)",
					"keep.option", "keep",
					"second.option", "second",
				).Error)
				var expectedRows []Option
				require.NoError(t, db.Table("options").Order("`key`").Find(&expectedRows).Error)
				if recovery {
					crashMySQLOptionMigrationAfterSwap(t, db)
				}

				var injected atomic.Bool
				var mutationErr error
				var artifacts mysqlOptionMigrationArtifacts
				const callbackName = "test:mysql-full-finalization-state"
				require.NoError(t, db.Callback().Raw().After("gorm:raw").Register(
					callbackName,
					func(tx *gorm.DB) {
						if !strings.Contains(
							tx.Statement.SQL.String(), "SET DEFAULT 'validated:",
						) || !injected.CompareAndSwap(false, true) {
							return
						}
						ctx := tx.Statement.Context
						if ctx == nil {
							ctx = context.Background()
						}
						var marker string
						if err := tx.Statement.ConnPool.QueryRowContext(ctx, `
SELECT column_comment
FROM information_schema.columns
WHERE table_schema = DATABASE() AND table_name = 'options' AND column_name = ?`,
							optionMigrationMarkerCol,
						).Scan(&marker); err != nil {
							mutationErr = err
							return
						}
						var ok bool
						artifacts, ok = parseMySQLOptionMigrationArtifacts("options", marker)
						if !ok {
							mutationErr = fmt.Errorf("parse active migration marker")
							return
						}
						artifacts.BackupTable = optionLegacyTablePrefix + artifacts.ID
						mutationErr = mutation.apply(
							ctx, tx.Statement.ConnPool, artifacts.BackupTable, artifacts,
						)
					},
				))
				t.Cleanup(func() {
					require.NoError(t, db.Callback().Raw().Remove(callbackName))
				})

				migrationErr := migrateOptionPrimaryKey(db.Session(&gorm.Session{NewDB: true}))

				require.True(t, injected.Load(), "the validated phase transition must be exercised")
				require.NoError(t, mutationErr)
				require.Error(t, migrationErr)
				restoredState, hasRestoredState, err := inspectMySQLOptionMigrationState(db, "options")
				require.NoError(t, err)
				require.True(t, hasRestoredState)
				assert.Equal(t, optionMigrationValidated, restoredState.Phase)
				mutation.verify(t, db, artifacts.BackupTable, artifacts)
				var activeRowsAfter, retainedRowsAfter []Option
				require.NoError(t, db.Table("options").Order("`key`").Find(&activeRowsAfter).Error)
				require.NoError(t, db.Table(artifacts.BackupTable).
					Order("`key`").Find(&retainedRowsAfter).Error)
				assert.Equal(t, expectedRows, activeRowsAfter)
				assert.Equal(t, expectedRows, retainedRowsAfter)

				afterFailure := captureMySQLOptionRecoverySnapshot(
					t, db, artifacts.BackupTable,
				)
				restartErr := migrateOptionPrimaryKey(
					db.Session(&gorm.Session{NewDB: true}),
				)
				if mutation.restartRecovers {
					require.NoError(t, restartErr)
					assertNoMySQLOptionMigrationArtifacts(t, db)
					var activeRows, retainedRows []Option
					require.NoError(t, db.Table("options").
						Order("`key`").Find(&activeRows).Error)
					require.NoError(t, db.Table(artifacts.BackupTable).
						Order("`key`").Find(&retainedRows).Error)
					assert.Equal(t, expectedRows, activeRows)
					assert.Equal(t, expectedRows, retainedRows)
				} else {
					require.Error(t, restartErr)
					assert.Equal(t, afterFailure, captureMySQLOptionRecoverySnapshot(
						t, db, artifacts.BackupTable,
					))
					mutation.verify(t, db, artifacts.BackupTable, artifacts)
				}
			})
		}
	}
}

func TestOptionPrimaryKeyMigrationRejectsExternalBridgeDeletionWithoutRestoreIntent(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 external restore-drift regression")
	}
	f := newMySQLOptionRestoreTestFixture(t)
	before := captureMySQLOptionRecoverySnapshot(t, f.db, f.artifacts.BackupTable)
	restoreDDL := observeMySQLOptionRestoreDDL(t, f.db)

	err := migrateOptionPrimaryKey(f.db.Session(&gorm.Session{NewDB: true}))

	require.Error(t, err)
	assert.Zero(t, restoreDDL.Load(), "external bridge deletion must not trigger recovery DDL")
	assert.Equal(t, before, captureMySQLOptionRecoverySnapshot(
		t, f.db, f.artifacts.BackupTable,
	))
}

func TestOptionPrimaryKeyMigrationRejectsMalformedOrStaleRestoreIntent(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 durable restore-intent regression")
	}
	for _, test := range []struct {
		name  string
		phase string
	}{
		{name: "malformed operation", phase: "restore/p/bad"},
		{name: "stale bridge state", phase: "restore/p/create_delete"},
		{name: "invalid target operation", phase: "restore/x/create_insert"},
		{name: "malformed forward operation", phase: "forward/i/bad"},
		{name: "stale forward bridge state", phase: "forward/b/drop_delete"},
		{name: "invalid forward target", phase: "forward/p/drop_insert"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newMySQLOptionRestoreTestFixture(t)
			intent := f.initialState
			intent.Phase = test.phase
			require.NoError(t, f.db.Exec(
				"ALTER TABLE `options` ALTER COLUMN "+quoteMySQLIdent(optionMigrationStateCol)+
					" SET DEFAULT '"+intent.value()+"'",
			).Error)
			before := captureMySQLOptionRecoverySnapshot(t, f.db, f.artifacts.BackupTable)
			restoreDDL := observeMySQLOptionRestoreDDL(t, f.db)

			err := migrateOptionPrimaryKey(f.db.Session(&gorm.Session{NewDB: true}))

			require.Error(t, err)
			assert.Zero(t, restoreDDL.Load(), "unproven restore intent must not trigger DDL")
			assert.Equal(t, before, captureMySQLOptionRecoverySnapshot(
				t, f.db, f.artifacts.BackupTable,
			))
		})
	}
}

func TestOptionPrimaryKeyMigrationRevalidatesAfterEveryFinalizationDDL(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 finalization DDL relock regression")
	}
	stages := []struct {
		name          string
		statement     func(mysqlOptionMigrationArtifacts) string
		expectedPhase string
		bridgePhase   string
		hasState      bool
	}{
		{
			name: "insert bridge drop", expectedPhase: "forward/i/drop_insert",
			bridgePhase: optionMigrationInsertGone, hasState: true,
			statement: func(artifacts mysqlOptionMigrationArtifacts) string {
				return "DROP TRIGGER " + quoteMySQLIdent(artifacts.InsertTrigger)
			},
		},
		{
			name: "update bridge drop", expectedPhase: "forward/u/drop_update",
			bridgePhase: optionMigrationUpdateGone, hasState: true,
			statement: func(artifacts mysqlOptionMigrationArtifacts) string {
				return "DROP TRIGGER " + quoteMySQLIdent(artifacts.UpdateTrigger)
			},
		},
		{
			name: "delete bridge drop", expectedPhase: "forward/b/drop_delete",
			bridgePhase: optionMigrationBridgesGone, hasState: true,
			statement: func(artifacts mysqlOptionMigrationArtifacts) string {
				return "DROP TRIGGER " + quoteMySQLIdent(artifacts.DeleteTrigger)
			},
		},
		{
			name: "terminal marker cleanup", bridgePhase: optionMigrationBridgesGone,
			statement: func(mysqlOptionMigrationArtifacts) string {
				return "DROP COLUMN " + quoteMySQLIdent(optionMigrationStateCol)
			},
		},
		{
			name: "terminal journal creation", expectedPhase: optionMigrationValidated,
			bridgePhase: optionMigrationValidated, hasState: true,
			statement: func(mysqlOptionMigrationArtifacts) string {
				return "CREATE TABLE " + quoteMySQLIdent(optionTerminalJournalTable)
			},
		},
	}

	for _, recovery := range []bool{false, true} {
		path := "ordinary migration"
		if recovery {
			path = "post-swap recovery"
		}
		for _, stage := range stages {
			t.Run(path+"/"+stage.name, func(t *testing.T) {
				db := useMigrationTestDB(t)
				require.NoError(t, db.Exec(
					"CREATE TABLE options ("+
						"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
						"`value` longtext,"+
						"`schema_marker` varchar(32) NOT NULL DEFAULT 'source-default',"+
						"KEY `idx_options_value` (`value`(16))"+
						") ENGINE=InnoDB",
				).Error)
				require.NoError(t, db.Exec(
					"INSERT INTO options (`key`, `value`) VALUES (?, ?), (?, ?)",
					"keep.option", "keep",
					"second.option", "second",
				).Error)
				var expectedRows []Option
				require.NoError(t, db.Table("options").Order("`key`").Find(&expectedRows).Error)
				if recovery {
					crashMySQLOptionMigrationAfterSwap(t, db)
				}

				var artifacts mysqlOptionMigrationArtifacts
				var injected atomic.Bool
				var mutationErr error
				const callbackName = "test:mysql-finalization-ddl-relock"
				require.NoError(t, db.Callback().Raw().After("gorm:raw").Register(
					callbackName,
					func(tx *gorm.DB) {
						statement := tx.Statement.SQL.String()
						if artifacts.ID == "" && strings.Contains(
							statement, "SET DEFAULT 'validated:",
						) {
							ctx := tx.Statement.Context
							if ctx == nil {
								ctx = context.Background()
							}
							var marker string
							if err := tx.Statement.ConnPool.QueryRowContext(ctx, `
SELECT column_comment
FROM information_schema.columns
WHERE table_schema = DATABASE() AND table_name = 'options' AND column_name = ?`,
								optionMigrationMarkerCol,
							).Scan(&marker); err != nil {
								mutationErr = err
								return
							}
							var ok bool
							artifacts, ok = parseMySQLOptionMigrationArtifacts("options", marker)
							if !ok {
								mutationErr = fmt.Errorf("parse active migration marker")
								return
							}
							artifacts.BackupTable = optionLegacyTablePrefix + artifacts.ID
						}
						if artifacts.ID == "" ||
							!strings.Contains(statement, stage.statement(artifacts)) ||
							!injected.CompareAndSwap(false, true) {
							return
						}
						ctx := tx.Statement.Context
						if ctx == nil {
							ctx = context.Background()
						}
						_, mutationErr = tx.Statement.ConnPool.ExecContext(ctx,
							"ALTER TABLE options "+
								"ADD COLUMN full_stage_post_ddl_drift varchar(32)",
						)
					},
				))
				t.Cleanup(func() {
					require.NoError(t, db.Callback().Raw().Remove(callbackName))
				})

				migrationErr := migrateOptionPrimaryKey(db.Session(&gorm.Session{NewDB: true}))

				require.True(t, injected.Load(), "the target finalization DDL must be exercised")
				require.NoError(t, mutationErr)
				require.Error(t, migrationErr)
				require.NotEmpty(t, artifacts.BackupTable)
				assert.True(t, db.Migrator().HasColumn("options", "full_stage_post_ddl_drift"))
				state, hasState, err := inspectMySQLOptionMigrationState(db, "options")
				require.NoError(t, err)
				assert.Equal(t, stage.hasState, hasState)
				if stage.hasState {
					assert.Equal(t, stage.expectedPhase, state.Phase)
					artifacts.SourceSchemaSHA256 = state.SourceSchemaSHA256
					artifacts.ActiveSchemaSHA256 = state.ActiveSchemaSHA256
				}
				triggers, err := inspectMySQLOptionMigrationTriggerCandidates(db, artifacts)
				require.NoError(t, err)
				expectedTriggers, validPhase := mysqlOptionMigrationBridgeNamesForPhase(
					artifacts, stage.bridgePhase,
				)
				require.True(t, validPhase)
				require.Len(t, triggers, len(expectedTriggers))
				actualNames := make([]string, len(triggers))
				for _, trigger := range triggers {
					assert.Equal(t, artifacts.BackupTable, trigger.TableName)
				}
				for i, trigger := range triggers {
					actualNames[i] = trigger.Name
				}
				assert.ElementsMatch(t, expectedTriggers, actualNames)
				var activeRows, retainedRows []Option
				require.NoError(t, db.Table("options").Order("`key`").Find(&activeRows).Error)
				require.NoError(t, db.Table(artifacts.BackupTable).
					Order("`key`").Find(&retainedRows).Error)
				assert.Equal(t, expectedRows, activeRows)
				assert.Equal(t, expectedRows, retainedRows)

				afterFailure := captureMySQLOptionRecoverySnapshot(
					t, db, artifacts.BackupTable,
				)
				restartErr := migrateOptionPrimaryKey(
					db.Session(&gorm.Session{NewDB: true}),
				)
				require.Error(t, restartErr)
				assert.Equal(t, afterFailure, captureMySQLOptionRecoverySnapshot(
					t, db, artifacts.BackupTable,
				))
			})
		}
	}
}

func TestOptionPrimaryKeyMigrationTerminalJournalCrashRecovery(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 terminal completion journal regression")
	}
	for _, crashWindow := range []struct {
		name              string
		statement         string
		journalAfterCrash bool
		snapshotRows      int64
		markersAfterCrash bool
	}{
		{
			name:              "journal creation",
			statement:         "CREATE TABLE `" + optionTerminalJournalTable + "`",
			journalAfterCrash: true,
			markersAfterCrash: true,
		},
		{
			name:              "journal snapshot",
			statement:         "INSERT INTO `" + optionTerminalJournalTable + "`",
			journalAfterCrash: true,
			snapshotRows:      1,
			markersAfterCrash: true,
		},
		{
			name:              "marker removal",
			statement:         "DROP COLUMN `" + optionMigrationStateCol + "`",
			journalAfterCrash: true,
			snapshotRows:      1,
		},
		{
			name:      "journal cleanup",
			statement: "DROP TABLE `" + optionTerminalJournalTable + "`",
		},
	} {
		t.Run(crashWindow.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options ("+
					"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
					"`value` longtext,"+
					"`schema_marker` varchar(32) NOT NULL DEFAULT 'source-default',"+
					"KEY `idx_options_value` (`value`(16))"+
					") ENGINE=InnoDB",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES (?, ?), (?, ?)",
				"keep.option", "keep",
				"second.option", "second",
			).Error)
			var expectedRows []Option
			require.NoError(t, db.Table("options").Order("`key`").Find(&expectedRows).Error)

			crashMySQLOptionFinalizationAt(t, db, crashWindow.statement)

			retained := mysqlOptionTerminalRetainedTable(t, db)
			assert.Equal(t, crashWindow.journalAfterCrash,
				mysqlOptionTerminalJournalExists(t, db))
			if crashWindow.journalAfterCrash {
				var snapshotRows int64
				require.NoError(t, db.Table(optionTerminalJournalTable).
					Count(&snapshotRows).Error)
				assert.Equal(t, crashWindow.snapshotRows, snapshotRows)
			}
			assert.Equal(t, crashWindow.markersAfterCrash,
				db.Migrator().HasColumn("options", optionMigrationMarkerCol))
			assert.Equal(t, crashWindow.markersAfterCrash,
				db.Migrator().HasColumn("options", optionMigrationStateCol))

			require.NoError(t, migrateOptionPrimaryKey(db.Session(&gorm.Session{NewDB: true})))
			assert.False(t, mysqlOptionTerminalJournalExists(t, db))
			var activeRows, retainedRows []Option
			require.NoError(t, db.Table("options").Order("`key`").Find(&activeRows).Error)
			require.NoError(t, db.Table(retained).Order("`key`").Find(&retainedRows).Error)
			assert.Equal(t, expectedRows, activeRows)
			assert.Equal(t, expectedRows, retainedRows)

			restartDDL := observeMySQLOptionRestoreDDL(t, db)
			require.NoError(t, migrateOptionPrimaryKey(db.Session(&gorm.Session{NewDB: true})))
			assert.Zero(t, restartDDL.Load(),
				"completed journal cleanup must make restart mutation-free")
		})
	}
}

func TestOptionPrimaryKeyMigrationRejectsWritesDuringTerminalJournalCreation(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 terminal journal row proof regression")
	}
	t.Run("bridged write is included in refreshed snapshot", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(
			"CREATE TABLE options ("+
				"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
				"`value` longtext"+
				") ENGINE=InnoDB",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
			"keep.option", "preserved",
		).Error)
		barrier := &optionMigrationSnapshotBarrier{
			Interface:         logger.Default.LogMode(logger.Silent),
			statement:         "CREATE TABLE " + quoteMySQLIdent(optionTerminalJournalTable),
			reached:           make(chan struct{}),
			continueMigration: make(chan struct{}),
		}
		migrationDone := make(chan error, 1)
		go func() {
			migrationDone <- migrateOptionPrimaryKey(
				db.Session(&gorm.Session{Logger: barrier, NewDB: true}),
			)
		}()
		select {
		case <-barrier.reached:
		case err := <-migrationDone:
			t.Fatalf("migration failed before terminal journal creation: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("migration did not reach terminal journal creation")
		}
		retained := mysqlOptionTerminalRetainedTable(t, db)
		id := mysqlOptionTerminalArtifactID(t, retained)
		tmpTable := optionMySQLTmpPrefix + id
		require.NoError(t, db.Exec(
			"CREATE VIEW "+quoteMySQLIdent(tmpTable)+
				" AS SELECT `key`, `value` FROM options",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO "+quoteMySQLIdent(retained)+" (`key`, `value`) VALUES (?, ?)",
			"journal.active", "private-active-value",
		).Error)
		require.NoError(t, db.Exec("DROP VIEW "+quoteMySQLIdent(tmpTable)).Error)
		close(barrier.continueMigration)

		require.NoError(t, <-migrationDone)
		assert.False(t, mysqlOptionTerminalJournalExists(t, db))
		for _, table := range []string{"options", retained} {
			var value string
			require.NoError(t, db.Table(table).Select("value").
				Where("`key` = ?", "journal.active").Scan(&value).Error)
			assert.Equal(t, "private-active-value", value)
		}
		beforeRestart := captureMySQLOptionRecoverySnapshot(t, db, retained)
		recorder := &migrationSQLRecorder{}
		require.NoError(t, migrateOptionPrimaryKey(db.Session(&gorm.Session{
			NewDB: true, Logger: recorder,
		})))
		assert.Empty(t, recorder.schemaMutations())
		assert.Equal(t, beforeRestart,
			captureMySQLOptionRecoverySnapshot(t, db, retained))
	})

	t.Run("direct retained-only write fails closed", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(
			"CREATE TABLE options ("+
				"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
				"`value` longtext"+
				") ENGINE=InnoDB",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
			"keep.option", "preserved",
		).Error)
		barrier := &optionMigrationSnapshotBarrier{
			Interface:         logger.Default.LogMode(logger.Silent),
			statement:         "CREATE TABLE " + quoteMySQLIdent(optionTerminalJournalTable),
			reached:           make(chan struct{}),
			continueMigration: make(chan struct{}),
		}
		migrationDone := make(chan error, 1)
		go func() {
			migrationDone <- migrateOptionPrimaryKey(
				db.Session(&gorm.Session{Logger: barrier, NewDB: true}),
			)
		}()
		select {
		case <-barrier.reached:
		case err := <-migrationDone:
			t.Fatalf("migration failed before terminal journal creation: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("migration did not reach terminal journal creation")
		}
		retained := mysqlOptionTerminalRetainedTable(t, db)
		require.NoError(t, db.Exec("TRUNCATE TABLE "+quoteMySQLIdent(retained)).Error)
		close(barrier.continueMigration)

		migrationErr := <-migrationDone

		require.Error(t, migrationErr)
		assert.NotContains(t, migrationErr.Error(), "preserved")
		assert.True(t, mysqlOptionTerminalJournalExists(t, db))
		beforeRestart := captureMySQLOptionRecoverySnapshot(t, db, retained)
		recorder := &migrationSQLRecorder{}
		restartErr := migrateOptionPrimaryKey(db.Session(&gorm.Session{
			NewDB: true, Logger: recorder,
		}))
		require.Error(t, restartErr)
		assert.NotContains(t, restartErr.Error(), "preserved")
		assert.Empty(t, recorder.schemaMutations())
		assert.Equal(t, beforeRestart,
			captureMySQLOptionRecoverySnapshot(t, db, retained))
	})
}

func TestOptionPrimaryKeyMigrationCreatesTerminalJournalBeforeBridgeTeardown(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 early terminal journal crash regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options ("+
			"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
			"`value` longtext"+
			") ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
		"keep.option", "preserved",
	).Error)

	crashMySQLOptionFinalizationAt(
		t, db, "CREATE TABLE "+quoteMySQLIdent(optionTerminalJournalTable),
	)

	retained := mysqlOptionTerminalRetainedTable(t, db)
	id := mysqlOptionTerminalArtifactID(t, retained)
	artifacts := mysqlOptionMigrationArtifacts{
		ID:            id,
		Marker:        optionMigrationMarker + "/" + id,
		TmpTable:      optionMySQLTmpPrefix + id,
		BackupTable:   retained,
		InsertTrigger: optionInsertTriggerPrefix + id,
		UpdateTrigger: optionUpdateTriggerPrefix + id,
		DeleteTrigger: optionDeleteTriggerPrefix + id,
	}
	state, hasState, err := inspectMySQLOptionMigrationState(db, "options")
	require.NoError(t, err)
	require.True(t, hasState)
	assert.Equal(t, optionMigrationValidated, state.Phase)
	triggers, err := inspectOwnedMySQLOptionMigrationTriggers(db, artifacts)
	require.NoError(t, err)
	assert.Len(t, triggers, 3)

	require.NoError(t, migrateOptionPrimaryKey(db.Session(&gorm.Session{NewDB: true})))
	assertNoMySQLOptionMigrationArtifacts(t, db)
}

func TestMySQLOptionRowsDigestUsesUnambiguousBinaryRepresentation(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 terminal row digest regression")
	}
	db := useMigrationTestDB(t)
	for _, table := range []string{"options_digest_left", "options_digest_right"} {
		require.NoError(t, db.Exec(
			"CREATE TABLE "+quoteMySQLIdent(table)+" ("+
				"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci,"+
				"`value` longtext"+
				") ENGINE=InnoDB",
		).Error)
	}
	digest := func(t *testing.T, table string) string {
		t.Helper()
		value, err := mysqlOptionRowsDigest(db, table, []string{"key", "value"})
		require.NoError(t, err)
		return value
	}
	semanticDigest := func(t *testing.T, table string) string {
		t.Helper()
		value, err := mysqlOptionSemanticRowsDigest(db, table, []string{"key", "value"})
		require.NoError(t, err)
		return value
	}
	reset := func(t *testing.T) {
		t.Helper()
		require.NoError(t, db.Exec("TRUNCATE TABLE options_digest_left").Error)
		require.NoError(t, db.Exec("TRUNCATE TABLE options_digest_right").Error)
	}

	t.Run("row order is irrelevant", func(t *testing.T) {
		reset(t)
		require.NoError(t, db.Exec(
			"INSERT INTO options_digest_left (`key`, `value`) VALUES (?, ?), (?, ?)",
			"alpha", "one", "beta", "two",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options_digest_right (`key`, `value`) VALUES (?, ?), (?, ?)",
			"beta", "two", "alpha", "one",
		).Error)
		assert.Equal(t, digest(t, "options_digest_left"), digest(t, "options_digest_right"))
	})
	t.Run("null differs from empty", func(t *testing.T) {
		reset(t)
		require.NoError(t, db.Exec(
			"INSERT INTO options_digest_left (`key`, `value`) VALUES (?, NULL)", "same",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options_digest_right (`key`, `value`) VALUES (?, '')", "same",
		).Error)
		assert.NotEqual(t, digest(t, "options_digest_left"), digest(t, "options_digest_right"))
		assert.NotEqual(t,
			semanticDigest(t, "options_digest_left"),
			semanticDigest(t, "options_digest_right"),
		)
	})
	t.Run("collation aliases remain distinct", func(t *testing.T) {
		reset(t)
		require.NoError(t, db.Exec(
			"INSERT INTO options_digest_left (`key`, `value`) VALUES (?, ?)", "Alias", "same",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options_digest_right (`key`, `value`) VALUES (?, ?)", "alias", "same",
		).Error)
		assert.NotEqual(t, digest(t, "options_digest_left"), digest(t, "options_digest_right"))
		assert.Equal(t,
			semanticDigest(t, "options_digest_left"),
			semanticDigest(t, "options_digest_right"),
		)
	})
	t.Run("cell boundaries remain distinct", func(t *testing.T) {
		reset(t)
		require.NoError(t, db.Exec(
			"INSERT INTO options_digest_left (`key`, `value`) VALUES (?, ?)", "ab", "c",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options_digest_right (`key`, `value`) VALUES (?, ?)", "a", "bc",
		).Error)
		assert.NotEqual(t, digest(t, "options_digest_left"), digest(t, "options_digest_right"))
	})
}

func TestOptionPrimaryKeyMigrationTerminalJournalRejectsPostMarkerDrift(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 terminal completion drift regression")
	}
	tests := []struct {
		name  string
		drift func(*testing.T, *gorm.DB, string, string)
	}{
		{
			name: "active schema",
			drift: func(t *testing.T, db *gorm.DB, _, _ string) {
				require.NoError(t, db.Exec(
					"ALTER TABLE options ADD COLUMN terminal_active_drift varchar(32)",
				).Error)
			},
		},
		{
			name: "retained schema",
			drift: func(t *testing.T, db *gorm.DB, retained, _ string) {
				require.NoError(t, db.Exec(
					"ALTER TABLE "+quoteMySQLIdent(retained)+
						" ADD COLUMN terminal_retained_drift varchar(32)",
				).Error)
			},
		},
		{
			name: "active row data",
			drift: func(t *testing.T, db *gorm.DB, _, _ string) {
				require.NoError(t, db.Exec(
					"UPDATE options SET `value` = ? WHERE `key` = ?",
					"private-active-row", "keep.option",
				).Error)
			},
		},
		{
			name: "retained row data",
			drift: func(t *testing.T, db *gorm.DB, retained, _ string) {
				require.NoError(t, db.Exec(
					"UPDATE "+quoteMySQLIdent(retained)+" SET `value` = ? WHERE `key` = ?",
					"private-retained-row", "keep.option",
				).Error)
			},
		},
		{
			name: "coordinated row data",
			drift: func(t *testing.T, db *gorm.DB, retained, _ string) {
				for _, table := range []string{"options", retained} {
					require.NoError(t, db.Exec(
						"UPDATE "+quoteMySQLIdent(table)+" SET `value` = ? WHERE `key` = ?",
						"private-coordinated-row", "keep.option",
					).Error)
				}
			},
		},
		{
			name: "sole primary key",
			drift: func(t *testing.T, db *gorm.DB, _, _ string) {
				require.NoError(t, db.Exec(
					"ALTER TABLE options DROP PRIMARY KEY, ADD UNIQUE KEY terminal_key (`key`)",
				).Error)
			},
		},
		{
			name: "incoming foreign key",
			drift: func(t *testing.T, db *gorm.DB, _, _ string) {
				require.NoError(t, db.Exec(
					"CREATE TABLE terminal_option_reference ("+
						"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
						"CONSTRAINT terminal_option_reference_fk FOREIGN KEY (`key`) "+
						"REFERENCES options (`key`)"+
						") ENGINE=InnoDB",
				).Error)
			},
		},
		{
			name: "active trigger",
			drift: func(t *testing.T, db *gorm.DB, _, _ string) {
				require.NoError(t, db.Exec(
					"CREATE TRIGGER terminal_option_trigger AFTER INSERT ON options "+
						"FOR EACH ROW SET @terminal_option_probe = NEW.`key`",
				).Error)
			},
		},
		{
			name: "candidate trigger",
			drift: func(t *testing.T, db *gorm.DB, _, id string) {
				candidateID := strings.Repeat("b", optionArtifactIDSize*2)
				if candidateID == id {
					candidateID = strings.Repeat("c", optionArtifactIDSize*2)
				}
				require.NoError(t, db.Exec(
					"CREATE TABLE terminal_option_trigger_owner (`key` varchar(191)) ENGINE=InnoDB",
				).Error)
				require.NoError(t, db.Exec(
					"CREATE TRIGGER "+quoteMySQLIdent(optionInsertTriggerPrefix+candidateID)+
						" AFTER INSERT ON terminal_option_trigger_owner "+
						"FOR EACH ROW SET @terminal_option_candidate = NEW.`key`",
				).Error)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options ("+
					"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
					"`value` longtext"+
					") ENGINE=InnoDB",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES ('keep.option', 'preserved')",
			).Error)
			crashMySQLOptionFinalizationAt(
				t, db, "DROP COLUMN `"+optionMigrationStateCol+"`",
			)
			retained := mysqlOptionTerminalRetainedTable(t, db)
			id := mysqlOptionTerminalArtifactID(t, retained)
			test.drift(t, db, retained, id)
			before := captureMySQLOptionRecoverySnapshot(t, db, retained)

			err := migrateOptionPrimaryKey(db.Session(&gorm.Session{NewDB: true}))

			require.Error(t, err)
			assert.NotContains(t, err.Error(), "private-")
			assert.Equal(t, before, captureMySQLOptionRecoverySnapshot(t, db, retained))
			assert.True(t, mysqlOptionTerminalJournalExists(t, db),
				"failed terminal proof must retain provenance")
		})
	}
}

func TestOptionPrimaryKeyMigrationTerminalJournalRejectsPostSnapshotRawAliasDrift(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 terminal raw alias drift regression")
	}
	for _, test := range []struct {
		name  string
		drift func(*testing.T, *gorm.DB, string)
	}{
		{
			name: "active",
			drift: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(
					"UPDATE options SET `key` = ? "+
						"WHERE OCTET_LENGTH(`key`) = ? AND BINARY `key` = BINARY ?",
					"ALIAS.OPTION", len("Alias.Option"), "Alias.Option",
				).Error)
			},
		},
		{
			name: "retained",
			drift: func(t *testing.T, db *gorm.DB, retained string) {
				require.NoError(t, db.Exec(
					"UPDATE "+quoteMySQLIdent(retained)+" SET `key` = ? "+
						"WHERE OCTET_LENGTH(`key`) = ? AND BINARY `key` = BINARY ?",
					"ALIAS.OPTION", len("Alias.Option"), "Alias.Option",
				).Error)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options ("+
					"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
					"`value` longtext"+
					") ENGINE=InnoDB",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
				"Alias.Option", "same",
			).Error)
			crashMySQLOptionFinalizationAt(
				t, db, "DROP COLUMN `"+optionMigrationStateCol+"`",
			)
			retained := mysqlOptionTerminalRetainedTable(t, db)
			test.drift(t, db, retained)
			before := captureMySQLOptionRecoverySnapshot(t, db, retained)
			recorder := &migrationSQLRecorder{}

			err := migrateOptionPrimaryKey(db.Session(&gorm.Session{
				NewDB: true, Logger: recorder,
			}))

			require.Error(t, err)
			assert.Empty(t, recorder.schemaMutations())
			assert.Equal(t, before, captureMySQLOptionRecoverySnapshot(t, db, retained))
			assert.True(t, mysqlOptionTerminalJournalExists(t, db))
		})
	}
}

func TestOptionPrimaryKeyMigrationTerminalJournalRejectsMissingActiveTable(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 terminal completion active-table regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (`key` varchar(191), `value` longtext) ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES ('keep.option', 'preserved')",
	).Error)
	crashMySQLOptionFinalizationAt(
		t, db, "DROP COLUMN `"+optionMigrationStateCol+"`",
	)
	retained := mysqlOptionTerminalRetainedTable(t, db)
	journalDDL, err := showCreateMySQLTable(db, optionTerminalJournalTable)
	require.NoError(t, err)
	retainedDDL, err := showCreateMySQLTable(db, retained)
	require.NoError(t, err)
	require.NoError(t, db.Exec("DROP TABLE options").Error)
	recorder := &migrationSQLRecorder{}

	err = migrateOptionPrimaryKey(db.Session(&gorm.Session{
		NewDB: true, Logger: recorder,
	}))

	require.Error(t, err)
	assert.Empty(t, recorder.schemaMutations(),
		"missing active terminal table must not trigger migration DDL")
	assert.False(t, db.Migrator().HasTable("options"))
	currentJournalDDL, showErr := showCreateMySQLTable(db, optionTerminalJournalTable)
	require.NoError(t, showErr)
	assert.Equal(t, journalDDL, currentJournalDDL)
	currentRetainedDDL, showErr := showCreateMySQLTable(db, retained)
	require.NoError(t, showErr)
	assert.Equal(t, retainedDDL, currentRetainedDDL)
}

func TestOptionPrimaryKeyMigrationRejectsMalformedOrMismatchedTerminalJournal(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 terminal completion journal ownership regression")
	}
	tests := []struct {
		name  string
		drift func(*testing.T, *gorm.DB, mysqlOptionTerminalJournal)
	}{
		{
			name: "malformed record",
			drift: func(t *testing.T, db *gorm.DB, _ mysqlOptionTerminalJournal) {
				require.NoError(t, db.Exec(
					"ALTER TABLE "+quoteMySQLIdent(optionTerminalJournalTable)+
						" COMMENT='malformed-private-record'",
				).Error)
			},
		},
		{
			name: "ownership mismatch",
			drift: func(t *testing.T, db *gorm.DB, journal mysqlOptionTerminalJournal) {
				journal.ID = strings.Repeat("a", optionArtifactIDSize*2)
				require.NoError(t, db.Exec(
					"ALTER TABLE "+quoteMySQLIdent(optionTerminalJournalTable)+
						" COMMENT='"+journal.value()+"'",
				).Error)
			},
		},
		{
			name: "phase mismatch",
			drift: func(t *testing.T, db *gorm.DB, journal mysqlOptionTerminalJournal) {
				journal.Phase = optionMigrationValidated
				require.NoError(t, db.Exec(
					"ALTER TABLE "+quoteMySQLIdent(optionTerminalJournalTable)+
						" COMMENT='"+journal.value()+"'",
				).Error)
			},
		},
		{
			name: "source digest mismatch",
			drift: func(t *testing.T, db *gorm.DB, journal mysqlOptionTerminalJournal) {
				journal.SourceSchemaSHA256 = strings.Repeat("a", sha256.Size*2)
				require.NoError(t, db.Exec(
					"ALTER TABLE "+quoteMySQLIdent(optionTerminalJournalTable)+
						" COMMENT='"+journal.value()+"'",
				).Error)
			},
		},
		{
			name: "terminal digest mismatch",
			drift: func(t *testing.T, db *gorm.DB, journal mysqlOptionTerminalJournal) {
				journal.TerminalSchemaSHA256 = strings.Repeat("a", sha256.Size*2)
				require.NoError(t, db.Exec(
					"ALTER TABLE "+quoteMySQLIdent(optionTerminalJournalTable)+
						" COMMENT='"+journal.value()+"'",
				).Error)
			},
		},
		{
			name: "malformed snapshot row",
			drift: func(t *testing.T, db *gorm.DB, journal mysqlOptionTerminalJournal) {
				require.NoError(t, db.Exec(
					"UPDATE "+quoteMySQLIdent(optionTerminalJournalTable)+
						" SET `active_rows_sha256` = ? WHERE `singleton` = 1",
					"malformed-private-record",
				).Error)
			},
		},
		{
			name: "active snapshot digest mismatch",
			drift: func(t *testing.T, db *gorm.DB, journal mysqlOptionTerminalJournal) {
				require.NoError(t, db.Exec(
					"UPDATE "+quoteMySQLIdent(optionTerminalJournalTable)+
						" SET `active_rows_sha256` = ? WHERE `singleton` = 1",
					strings.Repeat("a", sha256.Size*2),
				).Error)
			},
		},
		{
			name: "malformed retained raw snapshot",
			drift: func(t *testing.T, db *gorm.DB, _ mysqlOptionTerminalJournal) {
				require.NoError(t, db.Exec(
					"UPDATE "+quoteMySQLIdent(optionTerminalJournalTable)+
						" SET `retained_rows_sha256` = ? WHERE `singleton` = 1",
					"malformed-private-retained-record",
				).Error)
			},
		},
		{
			name: "malformed retained canonical snapshot",
			drift: func(t *testing.T, db *gorm.DB, _ mysqlOptionTerminalJournal) {
				require.NoError(t, db.Exec(
					"UPDATE "+quoteMySQLIdent(optionTerminalJournalTable)+
						" SET `retained_canonical_rows_sha256` = ? WHERE `singleton` = 1",
					"malformed-private-canonical-record",
				).Error)
			},
		},
		{
			name: "missing snapshot row",
			drift: func(t *testing.T, db *gorm.DB, _ mysqlOptionTerminalJournal) {
				require.NoError(t, db.Exec(
					"DELETE FROM "+quoteMySQLIdent(optionTerminalJournalTable),
				).Error)
			},
		},
		{
			name: "multiple snapshot rows",
			drift: func(t *testing.T, db *gorm.DB, journal mysqlOptionTerminalJournal) {
				require.NoError(t, db.Exec(
					"INSERT INTO "+quoteMySQLIdent(optionTerminalJournalTable)+
						" (`singleton`, `active_rows_sha256`, `retained_rows_sha256`, "+
						"`retained_canonical_rows_sha256`) VALUES (2, ?, ?, ?)",
					journal.ActiveRowsSHA256,
					journal.RetainedRowsSHA256,
					journal.RetainedCanonicalRowsSHA256,
				).Error)
			},
		},
		{
			name: "snapshot singleton mismatch",
			drift: func(t *testing.T, db *gorm.DB, _ mysqlOptionTerminalJournal) {
				require.NoError(t, db.Exec(
					"UPDATE "+quoteMySQLIdent(optionTerminalJournalTable)+
						" SET `singleton` = 2 WHERE `singleton` = 1",
				).Error)
			},
		},
		{
			name: "canonical digest comment collision",
			drift: func(t *testing.T, db *gorm.DB, _ mysqlOptionTerminalJournal) {
				require.NoError(t, db.Exec(
					"ALTER TABLE "+quoteMySQLIdent(optionTerminalJournalTable)+
						" MODIFY COLUMN `retained_canonical_rows_sha256`"+
						" CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL"+
						" COMMENT 'private-canonical-comment'",
				).Error)
			},
		},
		{
			name: "shape collision",
			drift: func(t *testing.T, db *gorm.DB, _ mysqlOptionTerminalJournal) {
				require.NoError(t, db.Exec(
					"ALTER TABLE "+quoteMySQLIdent(optionTerminalJournalTable)+
						" ADD COLUMN private_payload varchar(64)",
				).Error)
			},
		},
		{
			name: "index collision",
			drift: func(t *testing.T, db *gorm.DB, _ mysqlOptionTerminalJournal) {
				require.NoError(t, db.Exec(
					"ALTER TABLE "+quoteMySQLIdent(optionTerminalJournalTable)+
						" DROP PRIMARY KEY, ADD UNIQUE KEY journal_singleton (`singleton`)",
				).Error)
			},
		},
		{
			name: "table option collision",
			drift: func(t *testing.T, db *gorm.DB, _ mysqlOptionTerminalJournal) {
				require.NoError(t, db.Exec(
					"ALTER TABLE "+quoteMySQLIdent(optionTerminalJournalTable)+
						" ROW_FORMAT=DYNAMIC",
				).Error)
			},
		},
		{
			name: "journal trigger collision",
			drift: func(t *testing.T, db *gorm.DB, _ mysqlOptionTerminalJournal) {
				require.NoError(t, db.Exec(
					"CREATE TRIGGER terminal_journal_trigger AFTER INSERT ON "+
						quoteMySQLIdent(optionTerminalJournalTable)+
						" FOR EACH ROW SET @terminal_journal_probe = NEW.`singleton`",
				).Error)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options (`key` varchar(191), `value` longtext) ENGINE=InnoDB",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES ('keep.option', 'preserved')",
			).Error)
			crashMySQLOptionFinalizationAt(
				t, db, "DROP COLUMN `"+optionMigrationStateCol+"`",
			)
			retained := mysqlOptionTerminalRetainedTable(t, db)
			journal, exists, err := inspectMySQLOptionTerminalJournal(db)
			require.NoError(t, err)
			require.True(t, exists)
			test.drift(t, db, journal)
			before := captureMySQLOptionRecoverySnapshot(t, db, retained)
			recorder := &migrationSQLRecorder{}

			err = migrateOptionPrimaryKey(db.Session(&gorm.Session{
				NewDB: true, Logger: recorder,
			}))

			require.Error(t, err)
			assert.NotContains(t, err.Error(), "malformed-private-record")
			assert.Empty(t, recorder.schemaMutations(),
				"unproven journal must not trigger migration DDL")
			assert.Equal(t, before, captureMySQLOptionRecoverySnapshot(t, db, retained))
		})
	}
}

func TestOptionPrimaryKeyMigrationTerminalJournalMetadataFailureIsSanitized(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 terminal completion journal metadata regression")
	}
	tests := []struct {
		name          string
		statementPart string
		wantError     error
	}{
		{
			name:          "journal table",
			statementPart: "SELECT table_type, engine, table_collation, table_comment, create_options",
			wantError:     errMySQLOptionTerminalJournalInspection,
		},
		{
			name:          "global visibility",
			statementPart: "information_schema.user_privileges",
			wantError:     errMySQLMetadataVisibilityInspection,
		},
		{
			name:          "journal columns",
			statementPart: "SELECT column_name, data_type, column_type, is_nullable, column_default, extra",
			wantError:     errMySQLOptionTerminalJournalInspection,
		},
		{
			name:          "journal indexes",
			statementPart: "SELECT index_name, non_unique, seq_in_index, column_name, index_type",
			wantError:     errMySQLOptionTerminalJournalInspection,
		},
		{
			name:          "journal relations",
			statementPart: "referenced_table_name = ?))",
			wantError:     errMySQLOptionTerminalJournalInspection,
		},
		{
			name:          "journal rows",
			statementPart: optionTerminalJournalTable,
			wantError:     errMySQLOptionTerminalJournalInspection,
		},
		{
			name:          "journal snapshot",
			statementPart: "active_rows_sha256",
			wantError:     errMySQLOptionTerminalJournalInspection,
		},
		{name: "sole primary key", statementPart: "constraint_name = 'PRIMARY'"},
		{
			name:          "candidate triggers",
			statementPart: "SELECT trigger_name\nFROM information_schema.triggers",
		},
		{name: "incoming foreign keys", statementPart: "AS has_incoming_foreign_key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options (`key` varchar(191), `value` longtext) ENGINE=InnoDB",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES ('keep.option', 'preserved')",
			).Error)
			crashMySQLOptionFinalizationAt(
				t, db, "DROP COLUMN `"+optionMigrationStateCol+"`",
			)
			retained := mysqlOptionTerminalRetainedTable(t, db)
			before := captureMySQLOptionRecoverySnapshot(t, db, retained)
			privateDetail := "private terminal journal metadata payload"
			var injected atomic.Bool
			callback := func(tx *gorm.DB) {
				statement := tx.Statement.SQL.String()
				matches := strings.Contains(statement, test.statementPart)
				if test.name == "journal rows" {
					matches = strings.Contains(strings.ToUpper(statement), "SELECT COUNT") &&
						strings.Contains(statement, optionTerminalJournalTable)
				} else if test.name == "journal snapshot" {
					matches = strings.Contains(strings.ToUpper(statement), "SELECT") &&
						!strings.Contains(strings.ToUpper(statement), "SELECT COUNT") &&
						strings.Contains(statement, "active_rows_sha256") &&
						strings.Contains(statement, optionTerminalJournalTable)
				}
				if matches &&
					injected.CompareAndSwap(false, true) {
					tx.AddError(errors.New(privateDetail))
				}
			}
			rowCallback := "test:mysql-terminal-journal-row-metadata-failure"
			queryCallback := "test:mysql-terminal-journal-query-metadata-failure"
			queryAfterCallback := "test:mysql-terminal-journal-query-metadata-after-failure"
			rawCallback := "test:mysql-terminal-journal-raw-metadata-failure"
			require.NoError(t, db.Callback().Row().Before("gorm:row").Register(
				rowCallback, callback,
			))
			require.NoError(t, db.Callback().Query().Before("gorm:query").Register(
				queryCallback, callback,
			))
			require.NoError(t, db.Callback().Query().After("gorm:query").Register(
				queryAfterCallback, callback,
			))
			require.NoError(t, db.Callback().Raw().Before("gorm:raw").Register(
				rawCallback, callback,
			))
			t.Cleanup(func() {
				require.NoError(t, db.Callback().Row().Remove(rowCallback))
				require.NoError(t, db.Callback().Query().Remove(queryCallback))
				require.NoError(t, db.Callback().Query().Remove(queryAfterCallback))
				require.NoError(t, db.Callback().Raw().Remove(rawCallback))
			})
			recorder := &migrationSQLRecorder{}

			err := migrateOptionPrimaryKey(db.Session(&gorm.Session{
				NewDB: true, Logger: recorder,
			}))

			require.True(t, injected.Load(), "target metadata query must execute")
			require.ErrorIs(t, err, errMySQLOptionTerminalJournalRecoveryBlocked)
			if test.wantError != nil {
				require.ErrorIs(t, err, test.wantError)
			}
			assert.NotContains(t, err.Error(), privateDetail)
			assert.Empty(t, recorder.schemaMutations(),
				"journal metadata failure must not trigger migration DDL")
			assert.Equal(t, before, captureMySQLOptionRecoverySnapshot(t, db, retained))
		})
	}
}

func TestOptionPrimaryKeyMigrationFinalizationCrashProtocol(t *testing.T) {
	const (
		helperEnv          = "OPTION_MIGRATION_FINALIZATION_CRASH_HELPER"
		helperDSNEnv       = "OPTION_MIGRATION_FINALIZATION_CRASH_DSN"
		helperStatementEnv = "OPTION_MIGRATION_FINALIZATION_CRASH_STATEMENT"
	)
	if os.Getenv(helperEnv) == "1" {
		crashLogger := &optionMigrationFinalizationCrashLogger{
			Interface: logger.Default.LogMode(logger.Silent),
			statement: os.Getenv(helperStatementEnv),
		}
		db, err := gorm.Open(
			gormMySQL.Open(os.Getenv(helperDSNEnv)),
			&gorm.Config{Logger: crashLogger},
		)
		if err != nil {
			os.Exit(88)
		}
		common.SetMainDatabaseType(common.DatabaseTypeMySQL)
		if migrateOptionPrimaryKey(db) != nil {
			os.Exit(89)
		}
		os.Exit(90)
	}
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 finalization crash protocol")
	}

	createOptions := func(t *testing.T, db *gorm.DB) []Option {
		t.Helper()
		require.NoError(t, db.Exec(
			"CREATE TABLE options ("+
				"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
				"`value` longtext,"+
				"`schema_marker` varchar(32) NOT NULL DEFAULT 'source-default',"+
				"KEY `idx_options_value` (`value`(16))"+
				") ENGINE=InnoDB",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (`key`, `value`) VALUES (?, ?), (?, ?)",
			"keep.option", "keep",
			"second.option", "second",
		).Error)
		var rows []Option
		require.NoError(t, db.Table("options").Order("`key`").Find(&rows).Error)
		return rows
	}
	readArtifacts := func(
		t *testing.T,
		db *gorm.DB,
	) (mysqlOptionMigrationArtifacts, mysqlOptionMigrationState) {
		t.Helper()
		var marker string
		require.NoError(t, db.Raw(`
SELECT column_comment
FROM information_schema.columns
WHERE table_schema = DATABASE() AND table_name = 'options' AND column_name = ?`,
			optionMigrationMarkerCol).Scan(&marker).Error)
		artifacts, ok := parseMySQLOptionMigrationArtifacts("options", marker)
		require.True(t, ok)
		artifacts.BackupTable = optionLegacyTablePrefix + artifacts.ID
		state, hasState, err := inspectMySQLOptionMigrationState(db, "options")
		require.NoError(t, err)
		require.True(t, hasState)
		artifacts.SourceSchemaSHA256 = state.SourceSchemaSHA256
		artifacts.ActiveSchemaSHA256 = state.ActiveSchemaSHA256
		return artifacts, state
	}

	crashWindows := []struct {
		name          string
		statement     string
		expectedPhase string
		bridgePhase   string
	}{
		{
			name:          "insert bridge intent persisted before drop",
			statement:     "SET DEFAULT 'forward/i/drop_insert:",
			expectedPhase: "forward/i/drop_insert",
			bridgePhase:   optionMigrationPrepared,
		},
		{
			name:          "insert bridge dropped before phase",
			statement:     "DROP TRIGGER `" + optionInsertTriggerPrefix,
			expectedPhase: "forward/i/drop_insert",
			bridgePhase:   optionMigrationInsertGone,
		},
		{
			name:          "update bridge intent persisted before drop",
			statement:     "SET DEFAULT 'forward/u/drop_update:",
			expectedPhase: "forward/u/drop_update",
			bridgePhase:   optionMigrationInsertGone,
		},
		{
			name:          "update bridge dropped before phase",
			statement:     "DROP TRIGGER `" + optionUpdateTriggerPrefix,
			expectedPhase: "forward/u/drop_update",
			bridgePhase:   optionMigrationUpdateGone,
		},
		{
			name:          "delete bridge intent persisted before drop",
			statement:     "SET DEFAULT 'forward/b/drop_delete:",
			expectedPhase: "forward/b/drop_delete",
			bridgePhase:   optionMigrationUpdateGone,
		},
		{
			name:          "delete bridge dropped before phase",
			statement:     "DROP TRIGGER `" + optionDeleteTriggerPrefix,
			expectedPhase: "forward/b/drop_delete",
			bridgePhase:   optionMigrationBridgesGone,
		},
	}
	for _, recovery := range []bool{false, true} {
		path := "ordinary migration"
		if recovery {
			path = "post-swap recovery"
		}
		for _, crashWindow := range crashWindows {
			t.Run(path+"/"+crashWindow.name, func(t *testing.T) {
				db := useMigrationTestDB(t)
				expectedRows := createOptions(t, db)
				if recovery {
					crashMySQLOptionMigrationAfterSwap(t, db)
				}

				crashMySQLOptionFinalizationAt(t, db, crashWindow.statement)

				artifacts, state := readArtifacts(t, db)
				assert.Equal(t, crashWindow.expectedPhase, state.Phase)
				expectedBridges, ok := mysqlOptionMigrationBridgeNamesForPhase(
					artifacts, crashWindow.bridgePhase,
				)
				require.True(t, ok)
				triggers, err := inspectOwnedMySQLOptionMigrationTriggers(db, artifacts)
				require.NoError(t, err)
				actualBridges := make([]string, len(triggers))
				for i, trigger := range triggers {
					actualBridges[i] = trigger.Name
					assert.Equal(t, artifacts.BackupTable, trigger.TableName)
				}
				slices.Sort(actualBridges)
				slices.Sort(expectedBridges)
				assert.ElementsMatch(t, expectedBridges, actualBridges)

				require.NoError(t, migrateOptionPrimaryKey(
					db.Session(&gorm.Session{NewDB: true}),
				))
				assertNoMySQLOptionMigrationArtifacts(t, db)
				afterRestart := captureMySQLOptionRecoverySnapshot(
					t, db, artifacts.BackupTable,
				)
				require.NoError(t, migrateOptionPrimaryKey(
					db.Session(&gorm.Session{NewDB: true}),
				))
				assert.Equal(t, afterRestart, captureMySQLOptionRecoverySnapshot(
					t, db, artifacts.BackupTable,
				))
				var activeRows, retainedRows []Option
				require.NoError(t, db.Table("options").Order("`key`").Find(&activeRows).Error)
				require.NoError(t, db.Table(artifacts.BackupTable).
					Order("`key`").Find(&retainedRows).Error)
				assert.Equal(t, expectedRows, activeRows)
				assert.Equal(t, expectedRows, retainedRows)
			})
		}
	}

	stablePhases := []struct {
		name      string
		statement string
		phase     string
	}{
		{
			name:      "validated",
			statement: "SET DEFAULT 'validated:",
			phase:     optionMigrationValidated,
		},
		{
			name:      "insert removed",
			statement: "SET DEFAULT 'insert_removed:",
			phase:     optionMigrationInsertGone,
		},
		{
			name:      "update removed",
			statement: "SET DEFAULT 'update_removed:",
			phase:     optionMigrationUpdateGone,
		},
		{
			name:      "bridges removed",
			statement: "SET DEFAULT 'bridges_removed:",
			phase:     optionMigrationBridgesGone,
		},
	}
	for _, stable := range stablePhases {
		t.Run("stable phase resumes/"+stable.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			expectedRows := createOptions(t, db)

			crashMySQLOptionFinalizationAt(t, db, stable.statement)

			_, state := readArtifacts(t, db)
			require.Equal(t, stable.phase, state.Phase)
			require.NoError(t, migrateOptionPrimaryKey(
				db.Session(&gorm.Session{NewDB: true}),
			))
			assertNoMySQLOptionMigrationArtifacts(t, db)
			var activeRows []Option
			require.NoError(t, db.Table("options").Order("`key`").Find(&activeRows).Error)
			assert.Equal(t, expectedRows, activeRows)
		})
	}
}

func TestOptionPrimaryKeyMigrationRechecksMetadataVisibilityAtEveryFinalizationStage(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 finalization metadata visibility regression")
	}
	stages := []struct {
		name        string
		statement   string
		phase       string
		bridgePhase string
		hasState    bool
	}{
		{name: "validated phase", statement: "SET DEFAULT 'validated:",
			phase: optionMigrationValidated, bridgePhase: optionMigrationValidated, hasState: true},
		{name: "insert bridge intent", statement: "SET DEFAULT 'forward/i/drop_insert:",
			phase: "forward/i/drop_insert", bridgePhase: optionMigrationPrepared, hasState: true},
		{name: "insert bridge drop", statement: "DROP TRIGGER `" + optionInsertTriggerPrefix,
			phase: "forward/i/drop_insert", bridgePhase: optionMigrationInsertGone, hasState: true},
		{name: "insert removed phase", statement: "SET DEFAULT 'insert_removed:",
			phase: optionMigrationInsertGone, bridgePhase: optionMigrationInsertGone, hasState: true},
		{name: "update bridge intent", statement: "SET DEFAULT 'forward/u/drop_update:",
			phase: "forward/u/drop_update", bridgePhase: optionMigrationInsertGone, hasState: true},
		{name: "update bridge drop", statement: "DROP TRIGGER `" + optionUpdateTriggerPrefix,
			phase: "forward/u/drop_update", bridgePhase: optionMigrationUpdateGone, hasState: true},
		{name: "update removed phase", statement: "SET DEFAULT 'update_removed:",
			phase: optionMigrationUpdateGone, bridgePhase: optionMigrationUpdateGone, hasState: true},
		{name: "delete bridge intent", statement: "SET DEFAULT 'forward/b/drop_delete:",
			phase: "forward/b/drop_delete", bridgePhase: optionMigrationUpdateGone, hasState: true},
		{name: "delete bridge drop", statement: "DROP TRIGGER `" + optionDeleteTriggerPrefix,
			phase: "forward/b/drop_delete", bridgePhase: optionMigrationBridgesGone, hasState: true},
		{name: "bridges removed phase", statement: "SET DEFAULT 'bridges_removed:",
			phase: optionMigrationBridgesGone, bridgePhase: optionMigrationBridgesGone, hasState: true},
		{name: "terminal marker cleanup", statement: "DROP COLUMN `" + optionMigrationStateCol + "`",
			bridgePhase: optionMigrationBridgesGone},
		{name: "terminal journal creation",
			statement: "CREATE TABLE " + quoteMySQLIdent(optionTerminalJournalTable),
			phase:     optionMigrationValidated, bridgePhase: optionMigrationValidated, hasState: true},
		{name: "terminal journal cleanup",
			statement:   "DROP TABLE " + quoteMySQLIdent(optionTerminalJournalTable),
			bridgePhase: optionMigrationBridgesGone},
	}
	for i, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options ("+
					"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
					"`value` longtext,"+
					"KEY `idx_options_value` (`value`(16))"+
					") ENGINE=InnoDB",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
				"keep.option", "preserved",
			).Error)

			require.NoError(t, db.Exec(
				"CREATE TABLE option_recovery_external_target ("+
					"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL PRIMARY KEY"+
					") ENGINE=InnoDB",
			).Error)
			externalStem := fmt.Sprintf("caprestore%d", i)
			externalDefinition := createMySQLCrossSchemaOptionReference(
				t, db, externalStem, "option_recovery_external_target",
			)
			backup := crashMySQLOptionMigrationAfterSwap(t, db)
			var marker string
			require.NoError(t, db.Raw(`
SELECT column_comment
FROM information_schema.columns
WHERE table_schema = DATABASE() AND table_name = 'options' AND column_name = ?`,
				optionMigrationMarkerCol).Scan(&marker).Error)
			artifacts, ok := parseMySQLOptionMigrationArtifacts("options", marker)
			require.True(t, ok)
			artifacts.BackupTable = backup
			state, hasState, err := inspectMySQLOptionMigrationState(db, "options")
			require.NoError(t, err)
			require.True(t, hasState)
			artifacts.SourceSchemaSHA256 = state.SourceSchemaSHA256
			artifacts.ActiveSchemaSHA256 = state.ActiveSchemaSHA256

			var armed atomic.Bool
			var capabilityLost atomic.Bool
			var recoveryDDL atomic.Int32
			const observeCallback = "test:mysql-observe-recovery-after-capability-loss"
			require.NoError(t, db.Callback().Raw().After("gorm:raw").Register(
				observeCallback,
				func(tx *gorm.DB) {
					statement := strings.TrimSpace(tx.Statement.SQL.String())
					if capabilityLost.Load() &&
						(strings.HasPrefix(statement, "ALTER TABLE ") ||
							strings.HasPrefix(statement, "CREATE TRIGGER ") ||
							strings.HasPrefix(statement, "DROP TRIGGER ")) {
						recoveryDDL.Add(1)
					}
					if strings.Contains(statement, stage.statement) {
						armed.Store(true)
					}
				},
			))
			const rowCallback = "test:mysql-block-recovery-without-metadata-visibility"
			injectedErr := errors.New("injected finalization metadata failure")
			require.NoError(t, db.Callback().Row().Before("gorm:row").Register(
				rowCallback,
				func(tx *gorm.DB) {
					if armed.Load() &&
						strings.Contains(
							tx.Statement.SQL.String(), "information_schema.user_privileges",
						) {
						capabilityLost.Store(true)
						tx.AddError(injectedErr)
					}
				},
			))
			t.Cleanup(func() {
				require.NoError(t, db.Callback().Raw().Remove(observeCallback))
				require.NoError(t, db.Callback().Row().Remove(rowCallback))
			})

			err = migrateOptionPrimaryKey(db.Session(&gorm.Session{NewDB: true}))
			require.True(t, capabilityLost.Load(), "the following validation must lose capability")
			require.ErrorIs(t, err, errMySQLMetadataVisibilityInspection)
			assert.ErrorContains(t, err, "options migration recovery blocked")
			assert.NotContains(t, err.Error(), injectedErr.Error())
			assert.Zero(t, recoveryDDL.Load(), "capability loss must block every recovery DDL")
			assert.Equal(t, externalDefinition, captureMySQLCrossSchemaOptionReference(t, db, externalStem))
			currentState, currentHasState, stateErr := inspectMySQLOptionMigrationState(db, "options")
			require.NoError(t, stateErr)
			assert.Equal(t, stage.hasState, currentHasState)
			if stage.hasState {
				assert.Equal(t, stage.phase, currentState.Phase)
			}
			triggers, triggerErr := inspectOwnedMySQLOptionMigrationTriggers(db, artifacts)
			require.NoError(t, triggerErr)
			expectedTriggers, ok := mysqlOptionMigrationBridgeNamesForPhase(
				artifacts, stage.bridgePhase,
			)
			require.True(t, ok)
			actualTriggers := make([]string, len(triggers))
			for i, trigger := range triggers {
				actualTriggers[i] = trigger.Name
				assert.Equal(t, backup, trigger.TableName)
			}
			assert.ElementsMatch(t, expectedTriggers, actualTriggers)
			afterFailure := captureMySQLOptionRecoverySnapshot(t, db, backup)

			restartErr := migrateOptionPrimaryKey(db.Session(&gorm.Session{NewDB: true}))
			if stage.name == "terminal journal cleanup" {
				require.NoError(t, restartErr)
			} else {
				require.ErrorIs(t, restartErr, errMySQLMetadataVisibilityInspection)
			}
			assert.Zero(t, recoveryDDL.Load(), "a second restart must remain mutation-free")
			assert.Equal(t, afterFailure, captureMySQLOptionRecoverySnapshot(t, db, backup))
			assert.Equal(t, externalDefinition, captureMySQLCrossSchemaOptionReference(t, db, externalStem))
		})
	}
}

type mysqlOptionRestoreTestFixture struct {
	db               *gorm.DB
	artifacts        mysqlOptionMigrationArtifacts
	initialState     mysqlOptionMigrationState
	retainedTriggers []mysqlOptionTrigger
}

func newMySQLOptionRestoreTestFixture(t *testing.T) mysqlOptionRestoreTestFixture {
	t.Helper()
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options ("+
			"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
			"`value` longtext"+
			") ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
		"keep.option", "preserved",
	).Error)
	columns, err := mysqlWritableOptionColumns(db)
	require.NoError(t, err)
	sourceDDL, err := showCreateMySQLTable(db, "options")
	require.NoError(t, err)
	artifacts, err := newMySQLOptionMigrationArtifacts(
		bytes.NewReader(bytes.Repeat([]byte{0x6b}, optionArtifactIDSize)),
	)
	require.NoError(t, err)
	sourceDigest := sha256.Sum256([]byte(sourceDDL))
	artifacts.SourceSchemaSHA256 = hex.EncodeToString(sourceDigest[:])
	artifacts.BackupTable = optionLegacyTablePrefix + artifacts.ID
	require.NoError(t, createOwnedMySQLOptionMigrationTable(db, sourceDDL, artifacts))
	require.NoError(t, db.Exec(
		"ALTER TABLE "+quoteMySQLIdent(artifacts.TmpTable)+" ADD PRIMARY KEY (`key`)",
	).Error)
	require.NoError(t, recordMySQLOptionActiveSchema(db, &artifacts))
	require.NoError(t, db.Exec(
		"INSERT INTO "+quoteMySQLIdent(artifacts.TmpTable)+
			" (`key`, `value`) SELECT `key`, `value` FROM `options`",
	).Error)
	require.NoError(t, createMySQLOptionMigrationTriggers(db, columns, artifacts))
	require.NoError(t, swapOptionTables(db, artifacts.TmpTable, artifacts.BackupTable))
	initialState, hasState, err := inspectMySQLOptionMigrationState(db, "options")
	require.NoError(t, err)
	require.True(t, hasState)
	retainedTriggers, err := inspectOwnedMySQLOptionMigrationTriggers(db, artifacts)
	require.NoError(t, err)
	require.Len(t, retainedTriggers, 3)

	validated := initialState
	validated.Phase = optionMigrationValidated
	require.NoError(t, db.Exec(
		"ALTER TABLE `options` ALTER COLUMN "+quoteMySQLIdent(optionMigrationStateCol)+
			" SET DEFAULT '"+validated.value()+"'",
	).Error)
	require.NoError(t, db.Exec(
		"DROP TRIGGER "+quoteMySQLIdent(artifacts.InsertTrigger),
	).Error)
	return mysqlOptionRestoreTestFixture{
		db:               db,
		artifacts:        artifacts,
		initialState:     initialState,
		retainedTriggers: retainedTriggers,
	}
}

func observeMySQLOptionRestoreDDL(t *testing.T, db *gorm.DB) *atomic.Int32 {
	t.Helper()
	var count atomic.Int32
	callbackName := fmt.Sprintf("test:mysql-restore-ddl-observer:%p", &count)
	require.NoError(t, db.Callback().Raw().After("gorm:raw").Register(
		callbackName,
		func(tx *gorm.DB) {
			statement := strings.TrimSpace(tx.Statement.SQL.String())
			if strings.HasPrefix(statement, "ALTER TABLE ") ||
				strings.HasPrefix(statement, "DROP TRIGGER ") ||
				strings.HasPrefix(statement, "CREATE TRIGGER ") {
				count.Add(1)
			}
		},
	))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Raw().Remove(callbackName))
	})
	return &count
}

func TestOptionPrimaryKeyMigrationRestoreRejectsUnprovenFullStateBeforeDDL(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 restore full-state precondition regression")
	}
	type driftCase struct {
		name  string
		apply func(*testing.T, mysqlOptionRestoreTestFixture)
	}
	cases := []driftCase{
		{
			name: "active schema digest",
			apply: func(t *testing.T, f mysqlOptionRestoreTestFixture) {
				t.Helper()
				require.NoError(t, f.db.Exec(
					"ALTER TABLE options ADD COLUMN restore_active_drift varchar(32)",
				).Error)
			},
		},
		{
			name: "retained schema digest",
			apply: func(t *testing.T, f mysqlOptionRestoreTestFixture) {
				t.Helper()
				require.NoError(t, f.db.Exec(
					"ALTER TABLE "+quoteMySQLIdent(f.artifacts.BackupTable)+
						" ADD COLUMN restore_retained_drift varchar(32)",
				).Error)
			},
		},
		{
			name: "sole primary key",
			apply: func(t *testing.T, f mysqlOptionRestoreTestFixture) {
				t.Helper()
				require.NoError(t, f.db.Exec("ALTER TABLE options DROP PRIMARY KEY").Error)
			},
		},
		{
			name: "incoming foreign key",
			apply: func(t *testing.T, f mysqlOptionRestoreTestFixture) {
				t.Helper()
				require.NoError(t, f.db.Exec(
					"CREATE TABLE option_restore_reference ("+
						"`id` bigint PRIMARY KEY, "+
						"`option_key` varchar(191) CHARACTER SET utf8mb4 "+
						"COLLATE utf8mb4_unicode_ci NOT NULL, "+
						"CONSTRAINT fk_option_restore_reference "+
						"FOREIGN KEY (`option_key`) REFERENCES options (`key`)"+
						") ENGINE=InnoDB",
				).Error)
			},
		},
		{
			name: "bridge definition",
			apply: func(t *testing.T, f mysqlOptionRestoreTestFixture) {
				t.Helper()
				var original mysqlOptionTrigger
				for _, trigger := range f.retainedTriggers {
					if trigger.Name == f.artifacts.UpdateTrigger {
						original = trigger
					}
				}
				require.NotEmpty(t, original.Name)
				require.NoError(t, f.db.Exec(
					"DROP TRIGGER "+quoteMySQLIdent(original.Name),
				).Error)
				body, ok := strings.CutSuffix(strings.TrimSpace(original.Action), "END")
				require.True(t, ok)
				require.NoError(t, f.db.Exec(
					"CREATE TRIGGER "+quoteMySQLIdent(original.Name)+" "+
						original.Timing+" "+original.Event+" ON "+
						quoteMySQLIdent(f.artifacts.BackupTable)+" FOR EACH ROW "+
						body+" SET @new_api_restore_definition_drift = 1; END",
				).Error)
			},
		},
		{
			name: "bridge set",
			apply: func(t *testing.T, f mysqlOptionRestoreTestFixture) {
				t.Helper()
				require.NoError(t, f.db.Exec(
					"DROP TRIGGER "+quoteMySQLIdent(f.artifacts.UpdateTrigger),
				).Error)
			},
		},
		{
			name: "marker ownership",
			apply: func(t *testing.T, f mysqlOptionRestoreTestFixture) {
				t.Helper()
				require.NoError(t, f.db.Exec(
					"ALTER TABLE options MODIFY COLUMN "+
						quoteMySQLIdent(optionMigrationMarkerCol)+
						" TINYINT UNSIGNED NOT NULL DEFAULT 1 COMMENT 'foreign-owner'",
				).Error)
			},
		},
		{
			name: "state phase",
			apply: func(t *testing.T, f mysqlOptionRestoreTestFixture) {
				t.Helper()
				drifted := f.initialState
				drifted.Phase = optionMigrationBridgesGone
				require.NoError(t, f.db.Exec(
					"ALTER TABLE options ALTER COLUMN "+
						quoteMySQLIdent(optionMigrationStateCol)+
						" SET DEFAULT '"+drifted.value()+"'",
				).Error)
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newMySQLOptionRestoreTestFixture(t)
			test.apply(t, f)
			before := captureMySQLOptionRecoverySnapshot(
				t, f.db, f.artifacts.BackupTable,
			)
			restoreDDL := observeMySQLOptionRestoreDDL(t, f.db)

			err := restoreMySQLOptionFinalizationState(
				f.db, f.artifacts, f.initialState, f.retainedTriggers,
				mysqlOptionFinalizationRestorePoint{
					StatePhase:  optionMigrationValidated,
					BridgePhase: optionMigrationInsertGone,
				},
			)

			require.ErrorIs(t, err, errMySQLOptionFinalizationRecoveryBlocked)
			assert.Zero(t, restoreDDL.Load(), "unproven state must block restore DDL")
			assert.Equal(t, before, captureMySQLOptionRecoverySnapshot(
				t, f.db, f.artifacts.BackupTable,
			))
		})
	}
}

func TestOptionPrimaryKeyMigrationRestoreRevalidatesFullStateAfterEveryDDL(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 restore full-state postcondition regression")
	}
	t.Run("after state alter blocks trigger create", func(t *testing.T) {
		f := newMySQLOptionRestoreTestFixture(t)
		var injected atomic.Bool
		var creates atomic.Int32
		callbackName := "test:mysql-restore-after-state-alter"
		require.NoError(t, f.db.Callback().Raw().After("gorm:raw").Register(
			callbackName,
			func(tx *gorm.DB) {
				statement := strings.TrimSpace(tx.Statement.SQL.String())
				if strings.HasPrefix(statement, "CREATE TRIGGER ") {
					creates.Add(1)
				}
				if strings.Contains(statement, "SET DEFAULT 'restore/p/create_insert:") &&
					injected.CompareAndSwap(false, true) {
					ctx := tx.Statement.Context
					if ctx == nil {
						ctx = context.Background()
					}
					_, _ = tx.Statement.ConnPool.ExecContext(ctx,
						"ALTER TABLE options ADD COLUMN restore_post_state_drift varchar(32)")
				}
			},
		))
		t.Cleanup(func() {
			require.NoError(t, f.db.Callback().Raw().Remove(callbackName))
		})

		err := restoreMySQLOptionFinalizationState(
			f.db, f.artifacts, f.initialState, f.retainedTriggers,
			mysqlOptionFinalizationRestorePoint{
				StatePhase:  optionMigrationValidated,
				BridgePhase: optionMigrationInsertGone,
			},
		)

		require.True(t, injected.Load())
		require.ErrorIs(t, err, errMySQLOptionFinalizationRecoveryBlocked)
		assert.Zero(t, creates.Load(), "post-ALTER drift must block trigger recreation")
	})

	t.Run("after marker alter blocks trigger create", func(t *testing.T) {
		f := newMySQLOptionRestoreTestFixture(t)
		require.NoError(t, f.db.Exec(
			"DROP TRIGGER "+quoteMySQLIdent(f.artifacts.UpdateTrigger),
		).Error)
		require.NoError(t, f.db.Exec(
			"DROP TRIGGER "+quoteMySQLIdent(f.artifacts.DeleteTrigger),
		).Error)
		terminalDigest, err := mysqlOptionTerminalSchemaDigest(f.db, "options")
		require.NoError(t, err)
		require.NoError(t, f.db.Exec(
			"ALTER TABLE options DROP COLUMN "+quoteMySQLIdent(optionMigrationStateCol)+
				", DROP COLUMN "+quoteMySQLIdent(optionMigrationMarkerCol),
		).Error)
		var injected atomic.Bool
		var creates atomic.Int32
		callbackName := "test:mysql-restore-after-marker-alter"
		require.NoError(t, f.db.Callback().Raw().After("gorm:raw").Register(
			callbackName,
			func(tx *gorm.DB) {
				statement := strings.TrimSpace(tx.Statement.SQL.String())
				if strings.HasPrefix(statement, "CREATE TRIGGER ") {
					creates.Add(1)
				}
				if strings.Contains(statement, "ADD COLUMN "+
					quoteMySQLIdent(optionMigrationMarkerCol)) &&
					injected.CompareAndSwap(false, true) {
					ctx := tx.Statement.Context
					if ctx == nil {
						ctx = context.Background()
					}
					_, _ = tx.Statement.ConnPool.ExecContext(ctx,
						"ALTER TABLE "+quoteMySQLIdent(f.artifacts.BackupTable)+
							" ADD COLUMN restore_post_marker_drift varchar(32)")
				}
			},
		))
		t.Cleanup(func() {
			require.NoError(t, f.db.Callback().Raw().Remove(callbackName))
		})

		err = restoreMySQLOptionFinalizationState(
			f.db, f.artifacts, f.initialState, f.retainedTriggers,
			mysqlOptionFinalizationRestorePoint{
				StatePhase: optionMigrationBridgesGone, BridgePhase: optionMigrationBridgesGone,
				MarkersDropped: true, TerminalSchemaSHA256: terminalDigest,
			},
		)

		require.True(t, injected.Load())
		require.ErrorIs(t, err, errMySQLOptionFinalizationRecoveryBlocked)
		assert.Zero(t, creates.Load(), "post-marker drift must block trigger recreation")
	})

	t.Run("after nonterminal trigger create blocks the next create", func(t *testing.T) {
		f := newMySQLOptionRestoreTestFixture(t)
		require.NoError(t, f.db.Exec(
			"DROP TRIGGER "+quoteMySQLIdent(f.artifacts.UpdateTrigger),
		).Error)
		var injected atomic.Bool
		var creates atomic.Int32
		callbackName := "test:mysql-restore-after-trigger-create"
		require.NoError(t, f.db.Callback().Raw().After("gorm:raw").Register(
			callbackName,
			func(tx *gorm.DB) {
				statement := strings.TrimSpace(tx.Statement.SQL.String())
				if !strings.HasPrefix(statement, "CREATE TRIGGER ") {
					return
				}
				if creates.Add(1) == 1 && injected.CompareAndSwap(false, true) {
					ctx := tx.Statement.Context
					if ctx == nil {
						ctx = context.Background()
					}
					drifted := f.initialState
					drifted.Phase = optionMigrationBridgesGone
					_, _ = tx.Statement.ConnPool.ExecContext(ctx,
						"ALTER TABLE options ALTER COLUMN "+
							quoteMySQLIdent(optionMigrationStateCol)+
							" SET DEFAULT '"+drifted.value()+"'")
				}
			},
		))
		t.Cleanup(func() {
			require.NoError(t, f.db.Callback().Raw().Remove(callbackName))
		})

		err := restoreMySQLOptionFinalizationState(
			f.db, f.artifacts, f.initialState, f.retainedTriggers,
			mysqlOptionFinalizationRestorePoint{
				StatePhase:  optionMigrationValidated,
				BridgePhase: optionMigrationUpdateGone,
			},
		)

		require.True(t, injected.Load())
		require.ErrorIs(t, err, errMySQLOptionFinalizationRecoveryBlocked)
		assert.EqualValues(t, 1, creates.Load(),
			"post-CREATE drift must block every subsequent restore DDL")
	})

	t.Run("after final trigger create is still validated", func(t *testing.T) {
		f := newMySQLOptionRestoreTestFixture(t)
		var injected atomic.Bool
		var creates atomic.Int32
		callbackName := "test:mysql-restore-after-final-trigger-create"
		require.NoError(t, f.db.Callback().Raw().After("gorm:raw").Register(
			callbackName,
			func(tx *gorm.DB) {
				statement := strings.TrimSpace(tx.Statement.SQL.String())
				if strings.HasPrefix(statement, "CREATE TRIGGER ") &&
					creates.Add(1) == 1 && injected.CompareAndSwap(false, true) {
					ctx := tx.Statement.Context
					if ctx == nil {
						ctx = context.Background()
					}
					_, _ = tx.Statement.ConnPool.ExecContext(ctx,
						"ALTER TABLE options ADD COLUMN restore_final_drift varchar(32)")
				}
			},
		))
		t.Cleanup(func() {
			require.NoError(t, f.db.Callback().Raw().Remove(callbackName))
		})

		err := restoreMySQLOptionFinalizationState(
			f.db, f.artifacts, f.initialState, f.retainedTriggers,
			mysqlOptionFinalizationRestorePoint{
				StatePhase:  optionMigrationValidated,
				BridgePhase: optionMigrationInsertGone,
			},
		)

		require.True(t, injected.Load())
		require.ErrorIs(t, err, errMySQLOptionFinalizationRecoveryBlocked)
		assert.EqualValues(t, 1, creates.Load())
	})
}

func TestOptionPrimaryKeyMigrationValidRestoreCompletesAndSecondRestartIsMutationFree(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 valid restore and restart regression")
	}
	f := newMySQLOptionRestoreTestFixture(t)
	point := mysqlOptionFinalizationRestorePoint{
		StatePhase:  optionMigrationValidated,
		BridgePhase: optionMigrationInsertGone,
	}

	require.NoError(t, restoreMySQLOptionFinalizationState(
		f.db, f.artifacts, f.initialState, f.retainedTriggers, point,
	))
	restored, hasState, err := inspectMySQLOptionMigrationState(f.db, "options")
	require.NoError(t, err)
	require.True(t, hasState)
	assert.Equal(t, optionMigrationPrepared, restored.Phase)
	triggers, err := inspectOwnedMySQLOptionMigrationTriggers(f.db, f.artifacts)
	require.NoError(t, err)
	assert.Len(t, triggers, 3)

	require.NoError(t, migrateOptionPrimaryKey(f.db.Session(&gorm.Session{NewDB: true})))
	assertNoMySQLOptionMigrationArtifacts(t, f.db)
	beforeDDL, err := showCreateMySQLTable(f.db, "options")
	require.NoError(t, err)
	var beforeRows []Option
	require.NoError(t, f.db.Table("options").Order("`key`").Find(&beforeRows).Error)
	restartDDL := observeMySQLOptionRestoreDDL(t, f.db)

	require.NoError(t, migrateOptionPrimaryKey(f.db.Session(&gorm.Session{NewDB: true})))

	assert.Zero(t, restartDDL.Load(), "a completed valid restore must make the next restart mutation-free")
	afterDDL, err := showCreateMySQLTable(f.db, "options")
	require.NoError(t, err)
	assert.Equal(t, beforeDDL, afterDDL)
	var afterRows []Option
	require.NoError(t, f.db.Table("options").Order("`key`").Find(&afterRows).Error)
	assert.Equal(t, beforeRows, afterRows)
}

func TestOptionPrimaryKeyMigrationRestoreCrashRestartResumesAfterEveryDDL(t *testing.T) {
	const (
		helperEnv            = "OPTION_MIGRATION_RESTORE_CRASH_HELPER"
		helperDSNEnv         = "OPTION_MIGRATION_RESTORE_CRASH_DSN"
		helperMarkerEnv      = "OPTION_MIGRATION_RESTORE_CRASH_MARKER"
		helperSourceEnv      = "OPTION_MIGRATION_RESTORE_CRASH_SOURCE_DIGEST"
		helperActiveEnv      = "OPTION_MIGRATION_RESTORE_CRASH_ACTIVE_DIGEST"
		helperInitialPhase   = "OPTION_MIGRATION_RESTORE_CRASH_INITIAL_PHASE"
		helperStatePhase     = "OPTION_MIGRATION_RESTORE_CRASH_STATE_PHASE"
		helperBridgePhase    = "OPTION_MIGRATION_RESTORE_CRASH_BRIDGE_PHASE"
		helperMarkersDropped = "OPTION_MIGRATION_RESTORE_CRASH_MARKERS_DROPPED"
		helperTerminalDigest = "OPTION_MIGRATION_RESTORE_CRASH_TERMINAL_DIGEST"
		helperStatementEnv   = "OPTION_MIGRATION_RESTORE_CRASH_STATEMENT"
	)
	if os.Getenv(helperEnv) == "1" {
		crashLogger := &optionMigrationFinalizationCrashLogger{
			Interface: logger.Default.LogMode(logger.Silent),
			statement: os.Getenv(helperStatementEnv),
		}
		db, err := gorm.Open(
			gormMySQL.Open(os.Getenv(helperDSNEnv)),
			&gorm.Config{Logger: crashLogger},
		)
		if err != nil {
			os.Exit(88)
		}
		common.SetMainDatabaseType(common.DatabaseTypeMySQL)
		artifacts, ok := parseMySQLOptionMigrationArtifacts(
			"options", os.Getenv(helperMarkerEnv),
		)
		if !ok {
			os.Exit(89)
		}
		artifacts.BackupTable = optionLegacyTablePrefix + artifacts.ID
		artifacts.SourceSchemaSHA256 = os.Getenv(helperSourceEnv)
		artifacts.ActiveSchemaSHA256 = os.Getenv(helperActiveEnv)
		columns, err := mysqlWritableOptionColumnsFromTable(db, artifacts.BackupTable)
		if err != nil {
			os.Exit(90)
		}
		specs := mysqlOptionMigrationTriggerSpecs(columns, artifacts)
		retainedTriggers := make([]mysqlOptionTrigger, len(specs))
		for i, spec := range specs {
			retainedTriggers[i] = mysqlOptionTrigger{
				Name:      spec.Name,
				Event:     spec.Event,
				Timing:    "AFTER",
				TableName: artifacts.BackupTable,
				Action:    spec.Action,
			}
		}
		initialState := mysqlOptionMigrationState{
			Phase:              os.Getenv(helperInitialPhase),
			SourceSchemaSHA256: artifacts.SourceSchemaSHA256,
			ActiveSchemaSHA256: artifacts.ActiveSchemaSHA256,
		}
		point := mysqlOptionFinalizationRestorePoint{
			StatePhase:           os.Getenv(helperStatePhase),
			BridgePhase:          os.Getenv(helperBridgePhase),
			MarkersDropped:       os.Getenv(helperMarkersDropped) == "1",
			TerminalSchemaSHA256: os.Getenv(helperTerminalDigest),
		}
		if restoreMySQLOptionFinalizationState(
			db, artifacts, initialState, retainedTriggers, point,
		) != nil {
			os.Exit(91)
		}
		os.Exit(92)
	}
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 restore crash/restart regression")
	}

	type crashWindow struct {
		name                string
		statement           func(mysqlOptionMigrationArtifacts) string
		expectedStatePhase  string
		expectedBridgePhase string
		markersDropped      bool
	}
	restorePhase := func(operation string) string {
		return "restore/p/" + operation
	}
	windows := []crashWindow{
		{
			name: "persist insert intent",
			statement: func(mysqlOptionMigrationArtifacts) string {
				return "ALTER TABLE `options` ALTER COLUMN " +
					quoteMySQLIdent(optionMigrationStateCol) +
					" SET DEFAULT 'restore/p/create_insert:"
			},
			expectedStatePhase:  restorePhase("create_insert"),
			expectedBridgePhase: optionMigrationInsertGone,
		},
		{
			name: "insert trigger create from persisted intent",
			statement: func(artifacts mysqlOptionMigrationArtifacts) string {
				return "CREATE TRIGGER " + quoteMySQLIdent(artifacts.InsertTrigger)
			},
			expectedStatePhase:  restorePhase("create_insert"),
			expectedBridgePhase: optionMigrationPrepared,
		},
		{
			name: "persist clear intent after insert create",
			statement: func(mysqlOptionMigrationArtifacts) string {
				return "ALTER TABLE `options` ALTER COLUMN " +
					quoteMySQLIdent(optionMigrationStateCol) +
					" SET DEFAULT 'restore/p/clear:"
			},
			expectedStatePhase:  restorePhase("clear"),
			expectedBridgePhase: optionMigrationPrepared,
		},
		{
			name: "clear restore intent to target phase",
			statement: func(mysqlOptionMigrationArtifacts) string {
				return "ALTER TABLE `options` ALTER COLUMN " +
					quoteMySQLIdent(optionMigrationStateCol) +
					" SET DEFAULT 'prepared:"
			},
			expectedStatePhase:  optionMigrationPrepared,
			expectedBridgePhase: optionMigrationPrepared,
		},
		{
			name: "marker add persists delete intent",
			statement: func(mysqlOptionMigrationArtifacts) string {
				return "ALTER TABLE `options` ADD COLUMN " +
					quoteMySQLIdent(optionMigrationMarkerCol)
			},
			expectedStatePhase:  restorePhase("create_delete"),
			expectedBridgePhase: optionMigrationBridgesGone,
			markersDropped:      true,
		},
		{
			name: "delete trigger create from persisted intent",
			statement: func(artifacts mysqlOptionMigrationArtifacts) string {
				return "CREATE TRIGGER " + quoteMySQLIdent(artifacts.DeleteTrigger)
			},
			expectedStatePhase:  restorePhase("create_delete"),
			expectedBridgePhase: optionMigrationUpdateGone,
			markersDropped:      true,
		},
		{
			name: "persist update intent",
			statement: func(mysqlOptionMigrationArtifacts) string {
				return "ALTER TABLE `options` ALTER COLUMN " +
					quoteMySQLIdent(optionMigrationStateCol) +
					" SET DEFAULT 'restore/p/create_update:"
			},
			expectedStatePhase:  restorePhase("create_update"),
			expectedBridgePhase: optionMigrationUpdateGone,
			markersDropped:      true,
		},
		{
			name: "update trigger create from persisted intent",
			statement: func(artifacts mysqlOptionMigrationArtifacts) string {
				return "CREATE TRIGGER " + quoteMySQLIdent(artifacts.UpdateTrigger)
			},
			expectedStatePhase:  restorePhase("create_update"),
			expectedBridgePhase: optionMigrationInsertGone,
			markersDropped:      true,
		},
		{
			name: "persist insert intent after update create",
			statement: func(mysqlOptionMigrationArtifacts) string {
				return "ALTER TABLE `options` ALTER COLUMN " +
					quoteMySQLIdent(optionMigrationStateCol) +
					" SET DEFAULT 'restore/p/create_insert:"
			},
			expectedStatePhase:  restorePhase("create_insert"),
			expectedBridgePhase: optionMigrationInsertGone,
			markersDropped:      true,
		},
		{
			name: "insert trigger create after marker recreation",
			statement: func(artifacts mysqlOptionMigrationArtifacts) string {
				return "CREATE TRIGGER " + quoteMySQLIdent(artifacts.InsertTrigger)
			},
			expectedStatePhase:  restorePhase("create_insert"),
			expectedBridgePhase: optionMigrationPrepared,
			markersDropped:      true,
		},
		{
			name: "persist clear intent after marker recreation",
			statement: func(mysqlOptionMigrationArtifacts) string {
				return "ALTER TABLE `options` ALTER COLUMN " +
					quoteMySQLIdent(optionMigrationStateCol) +
					" SET DEFAULT 'restore/p/clear:"
			},
			expectedStatePhase:  restorePhase("clear"),
			expectedBridgePhase: optionMigrationPrepared,
			markersDropped:      true,
		},
		{
			name: "clear restore intent after marker recreation",
			statement: func(mysqlOptionMigrationArtifacts) string {
				return "ALTER TABLE `options` ALTER COLUMN " +
					quoteMySQLIdent(optionMigrationStateCol) +
					" SET DEFAULT 'prepared:"
			},
			expectedStatePhase:  optionMigrationPrepared,
			expectedBridgePhase: optionMigrationPrepared,
			markersDropped:      true,
		},
	}
	for _, window := range windows {
		t.Run(window.name, func(t *testing.T) {
			f := newMySQLOptionRestoreTestFixture(t)
			var expectedRows []Option
			require.NoError(t, f.db.Table("options").Order("`key`").Find(&expectedRows).Error)
			point := mysqlOptionFinalizationRestorePoint{
				StatePhase:  optionMigrationValidated,
				BridgePhase: optionMigrationInsertGone,
			}
			if window.markersDropped {
				require.NoError(t, f.db.Exec(
					"DROP TRIGGER "+quoteMySQLIdent(f.artifacts.UpdateTrigger),
				).Error)
				require.NoError(t, f.db.Exec(
					"DROP TRIGGER "+quoteMySQLIdent(f.artifacts.DeleteTrigger),
				).Error)
				terminalDigest, err := mysqlOptionTerminalSchemaDigest(f.db, "options")
				require.NoError(t, err)
				require.NoError(t, f.db.Exec(
					"ALTER TABLE `options` DROP COLUMN "+
						quoteMySQLIdent(optionMigrationStateCol)+
						", DROP COLUMN "+quoteMySQLIdent(optionMigrationMarkerCol),
				).Error)
				point = mysqlOptionFinalizationRestorePoint{
					StatePhase:           optionMigrationBridgesGone,
					BridgePhase:          optionMigrationBridgesGone,
					MarkersDropped:       true,
					TerminalSchemaSHA256: terminalDigest,
				}
			}

			crashMySQLOptionRestoreAt(
				t, f.db, f.artifacts, f.initialState, point,
				window.statement(f.artifacts),
			)

			state, hasState, err := inspectMySQLOptionMigrationState(f.db, "options")
			require.NoError(t, err)
			require.True(t, hasState)
			require.Equal(t, window.expectedStatePhase, state.Phase)
			require.NoError(t, validateMySQLOptionFinalizationStateForBridgePhase(
				f.db, f.artifacts, state, window.expectedBridgePhase,
			))

			require.NoError(t, migrateOptionPrimaryKey(
				f.db.Session(&gorm.Session{NewDB: true}),
			))
			assertNoMySQLOptionMigrationArtifacts(t, f.db)
			var activeRows []Option
			require.NoError(t, f.db.Table("options").Order("`key`").Find(&activeRows).Error)
			assert.Equal(t, expectedRows, activeRows)
		})
	}
}

func TestOptionPrimaryKeyMigrationRestoreErrorsRemainSanitizedWhenJoined(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 restore error sanitization regression")
	}
	f := newMySQLOptionRestoreTestFixture(t)
	privateDetail := "private restore SQL payload secret"
	var failed atomic.Bool
	callbackName := "test:mysql-restore-ddl-error-sanitization"
	require.NoError(t, f.db.Callback().Raw().Before("gorm:raw").Register(
		callbackName,
		func(tx *gorm.DB) {
			if strings.Contains(tx.Statement.SQL.String(), "SET DEFAULT 'restore/p/create_insert:") &&
				failed.CompareAndSwap(false, true) {
				tx.AddError(errors.New(privateDetail))
			}
		},
	))
	t.Cleanup(func() {
		require.NoError(t, f.db.Callback().Raw().Remove(callbackName))
	})

	restoreErr := restoreMySQLOptionFinalizationState(
		f.db, f.artifacts, f.initialState, f.retainedTriggers,
		mysqlOptionFinalizationRestorePoint{
			StatePhase:  optionMigrationValidated,
			BridgePhase: optionMigrationInsertGone,
		},
	)
	originalErr := errors.New("original finalization failure")
	joined := errors.Join(originalErr, restoreErr)

	require.True(t, failed.Load(), "the restore ALTER must receive the injected database error")
	require.ErrorIs(t, joined, originalErr)
	require.ErrorIs(t, joined, errMySQLOptionFinalizationRecoveryBlocked)
	assert.NotContains(t, joined.Error(), privateDetail)
	assert.NotContains(t, joined.Error(), "SET DEFAULT")
}

func TestOptionPrimaryKeyMigrationGuardsEveryRecoveryDDL(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 recovery DDL guard regression")
	}
	type recoveryCase struct {
		name        string
		targetGuard int32
		prepare     func(*testing.T, *gorm.DB, mysqlOptionMigrationArtifacts,
			mysqlOptionMigrationState, map[string]mysqlOptionTrigger,
		) (mysqlOptionMigrationState, []mysqlOptionTrigger, bool)
	}
	triggerNames := []struct {
		name string
		pick func(mysqlOptionMigrationArtifacts) string
	}{
		{name: "insert", pick: func(artifacts mysqlOptionMigrationArtifacts) string {
			return artifacts.InsertTrigger
		}},
		{name: "update", pick: func(artifacts mysqlOptionMigrationArtifacts) string {
			return artifacts.UpdateTrigger
		}},
		{name: "delete", pick: func(artifacts mysqlOptionMigrationArtifacts) string {
			return artifacts.DeleteTrigger
		}},
	}
	otherTriggerNames := map[string][]string{
		"insert": {"update", "delete"},
		"update": {"insert", "delete"},
		"delete": {"insert", "update"},
	}
	dropTrigger := func(t *testing.T, db *gorm.DB, name string) {
		t.Helper()
		require.NoError(t, db.Exec("DROP TRIGGER "+quoteMySQLIdent(name)).Error)
	}
	createTrigger := func(t *testing.T, db *gorm.DB, table string, trigger mysqlOptionTrigger) {
		t.Helper()
		require.NoError(t, db.Exec(
			"CREATE TRIGGER "+quoteMySQLIdent(trigger.Name)+" "+trigger.Timing+
				" "+trigger.Event+" ON "+quoteMySQLIdent(table)+
				" FOR EACH ROW "+trigger.Action,
		).Error)
	}
	setPhase := func(
		t *testing.T,
		db *gorm.DB,
		state mysqlOptionMigrationState,
		phase string,
	) mysqlOptionMigrationState {
		t.Helper()
		state.Phase = phase
		require.NoError(t, db.Exec(
			"ALTER TABLE `options` ALTER COLUMN "+quoteMySQLIdent(optionMigrationStateCol)+
				" SET DEFAULT '"+state.value()+"'",
		).Error)
		return state
	}
	orderedTriggers := func(
		artifacts mysqlOptionMigrationArtifacts,
		byName map[string]mysqlOptionTrigger,
		names ...string,
	) []mysqlOptionTrigger {
		result := make([]mysqlOptionTrigger, 0, len(names))
		for _, name := range names {
			switch name {
			case "insert":
				result = append(result, byName[artifacts.InsertTrigger])
			case "update":
				result = append(result, byName[artifacts.UpdateTrigger])
			case "delete":
				result = append(result, byName[artifacts.DeleteTrigger])
			}
		}
		return result
	}

	cases := []recoveryCase{
		{
			name:        "restore state alter",
			targetGuard: 1,
			prepare: func(t *testing.T, db *gorm.DB, artifacts mysqlOptionMigrationArtifacts,
				state mysqlOptionMigrationState, byName map[string]mysqlOptionTrigger,
			) (mysqlOptionMigrationState, []mysqlOptionTrigger, bool) {
				setPhase(t, db, state, optionMigrationValidated)
				dropTrigger(t, db, artifacts.InsertTrigger)
				return state, orderedTriggers(
					artifacts, byName, "insert", "update", "delete",
				), false
			},
		},
		{
			name:        "restore markers alter",
			targetGuard: 1,
			prepare: func(t *testing.T, db *gorm.DB, artifacts mysqlOptionMigrationArtifacts,
				state mysqlOptionMigrationState, byName map[string]mysqlOptionTrigger,
			) (mysqlOptionMigrationState, []mysqlOptionTrigger, bool) {
				for _, name := range artifacts.triggerNames() {
					dropTrigger(t, db, name)
				}
				require.NoError(t, db.Exec(
					"ALTER TABLE `options` DROP COLUMN "+quoteMySQLIdent(optionMigrationStateCol)+
						", DROP COLUMN "+quoteMySQLIdent(optionMigrationMarkerCol),
				).Error)
				return state, orderedTriggers(
					artifacts, byName, "insert", "update", "delete",
				), true
			},
		},
	}
	for _, candidate := range triggerNames {
		candidate := candidate
		cases = append(cases,
			recoveryCase{
				name:        "drop stale " + candidate.name + " bridge",
				targetGuard: 2,
				prepare: func(t *testing.T, db *gorm.DB, artifacts mysqlOptionMigrationArtifacts,
					state mysqlOptionMigrationState, byName map[string]mysqlOptionTrigger,
				) (mysqlOptionMigrationState, []mysqlOptionTrigger, bool) {
					target := candidate.pick(artifacts)
					for _, name := range artifacts.triggerNames() {
						if name != target {
							dropTrigger(t, db, name)
						}
					}
					state = setPhase(t, db, state, optionMigrationBridgesGone)
					return state, nil, false
				},
			},
			recoveryCase{
				name:        "drop drifted " + candidate.name + " bridge",
				targetGuard: 2,
				prepare: func(t *testing.T, db *gorm.DB, artifacts mysqlOptionMigrationArtifacts,
					state mysqlOptionMigrationState, byName map[string]mysqlOptionTrigger,
				) (mysqlOptionMigrationState, []mysqlOptionTrigger, bool) {
					target := candidate.pick(artifacts)
					original := byName[target]
					dropTrigger(t, db, target)
					body, ok := strings.CutSuffix(strings.TrimSpace(original.Action), "END")
					require.True(t, ok)
					original.Action = body + " SET @new_api_recovery_guard_drift = 1; END"
					createTrigger(t, db, artifacts.BackupTable, original)
					setPhase(t, db, state, optionMigrationValidated)
					return state, append(
						[]mysqlOptionTrigger{byName[target]},
						orderedTriggers(
							artifacts, byName,
							otherTriggerNames[candidate.name]...,
						)...,
					), false
				},
			},
			recoveryCase{
				name:        "create drifted " + candidate.name + " bridge",
				targetGuard: 3,
				prepare: func(t *testing.T, db *gorm.DB, artifacts mysqlOptionMigrationArtifacts,
					state mysqlOptionMigrationState, byName map[string]mysqlOptionTrigger,
				) (mysqlOptionMigrationState, []mysqlOptionTrigger, bool) {
					target := candidate.pick(artifacts)
					original := byName[target]
					dropTrigger(t, db, target)
					body, ok := strings.CutSuffix(strings.TrimSpace(original.Action), "END")
					require.True(t, ok)
					original.Action = body + " SET @new_api_recovery_guard_drift = 1; END"
					createTrigger(t, db, artifacts.BackupTable, original)
					setPhase(t, db, state, optionMigrationValidated)
					return state, append(
						[]mysqlOptionTrigger{byName[target]},
						orderedTriggers(
							artifacts, byName,
							otherTriggerNames[candidate.name]...,
						)...,
					), false
				},
			},
			recoveryCase{
				name:        "create missing " + candidate.name + " bridge",
				targetGuard: 2,
				prepare: func(t *testing.T, db *gorm.DB, artifacts mysqlOptionMigrationArtifacts,
					state mysqlOptionMigrationState, byName map[string]mysqlOptionTrigger,
				) (mysqlOptionMigrationState, []mysqlOptionTrigger, bool) {
					target := candidate.pick(artifacts)
					dropTrigger(t, db, target)
					setPhase(t, db, state, optionMigrationValidated)
					return state, append(
						[]mysqlOptionTrigger{byName[target]},
						orderedTriggers(
							artifacts, byName,
							otherTriggerNames[candidate.name]...,
						)...,
					), false
				},
			},
		)
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options ("+
					"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
					"`value` longtext"+
					") ENGINE=InnoDB",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
				"keep.option", "preserved",
			).Error)
			columns, err := mysqlWritableOptionColumns(db)
			require.NoError(t, err)
			sourceDDL, err := showCreateMySQLTable(db, "options")
			require.NoError(t, err)
			artifacts, err := newMySQLOptionMigrationArtifacts(
				bytes.NewReader(bytes.Repeat([]byte{0x5a}, optionArtifactIDSize)),
			)
			require.NoError(t, err)
			sourceDigest := sha256.Sum256([]byte(sourceDDL))
			artifacts.SourceSchemaSHA256 = hex.EncodeToString(sourceDigest[:])
			artifacts.BackupTable = optionLegacyTablePrefix + artifacts.ID
			require.NoError(t, createOwnedMySQLOptionMigrationTable(db, sourceDDL, artifacts))
			require.NoError(t, db.Exec(
				"ALTER TABLE "+quoteMySQLIdent(artifacts.TmpTable)+" ADD PRIMARY KEY (`key`)",
			).Error)
			require.NoError(t, recordMySQLOptionActiveSchema(db, &artifacts))
			require.NoError(t, db.Exec(
				"INSERT INTO "+quoteMySQLIdent(artifacts.TmpTable)+
					" (`key`, `value`) SELECT `key`, `value` FROM `options`",
			).Error)
			require.NoError(t, createMySQLOptionMigrationTriggers(db, columns, artifacts))
			require.NoError(t, swapOptionTables(db, artifacts.TmpTable, artifacts.BackupTable))
			state, hasState, err := inspectMySQLOptionMigrationState(db, "options")
			require.NoError(t, err)
			require.True(t, hasState)
			triggers, err := inspectOwnedMySQLOptionMigrationTriggers(db, artifacts)
			require.NoError(t, err)
			require.Len(t, triggers, 3)
			byName := make(map[string]mysqlOptionTrigger, len(triggers))
			for _, trigger := range triggers {
				byName[trigger.Name] = trigger
			}
			initialState, retained, markersDropped := test.prepare(
				t, db, artifacts, state, byName,
			)
			beforeRestore := captureMySQLOptionRecoverySnapshot(t, db, artifacts.BackupTable)

			var guardCount atomic.Int32
			var capabilityLost atomic.Bool
			var recoveryDDL atomic.Int32
			var ddlAtCapabilityLoss atomic.Int32
			privateErr := errors.New("private injected recovery capability detail")
			const rowCallback = "test:mysql-recovery-ddl-capability-loss"
			require.NoError(t, db.Callback().Row().Before("gorm:row").Register(
				rowCallback,
				func(tx *gorm.DB) {
					if !strings.Contains(
						tx.Statement.SQL.String(), "information_schema.user_privileges",
					) {
						return
					}
					if capabilityLost.Load() || guardCount.Add(1) == test.targetGuard {
						ddlAtCapabilityLoss.Store(recoveryDDL.Load())
						capabilityLost.Store(true)
						tx.AddError(privateErr)
					}
				},
			))
			const rawCallback = "test:mysql-recovery-ddl-observer"
			require.NoError(t, db.Callback().Raw().After("gorm:raw").Register(
				rawCallback,
				func(tx *gorm.DB) {
					statement := strings.TrimSpace(tx.Statement.SQL.String())
					if strings.HasPrefix(statement, "ALTER TABLE ") ||
						strings.HasPrefix(statement, "DROP TRIGGER ") ||
						strings.HasPrefix(statement, "CREATE TRIGGER ") {
						recoveryDDL.Add(1)
					}
				},
			))
			t.Cleanup(func() {
				require.NoError(t, db.Callback().Row().Remove(rowCallback))
				require.NoError(t, db.Callback().Raw().Remove(rawCallback))
			})

			originalErr := errors.New("original finalization failure")
			driftBlocked := strings.HasPrefix(test.name, "drop stale ") ||
				strings.HasPrefix(test.name, "drop drifted ") ||
				strings.HasPrefix(test.name, "create drifted ") ||
				strings.HasPrefix(test.name, "create missing update ") ||
				strings.HasPrefix(test.name, "create missing delete ")
			point := mysqlOptionFinalizationRestorePoint{}
			if markersDropped {
				point.StatePhase = optionMigrationBridgesGone
				point.BridgePhase = optionMigrationBridgesGone
				point.MarkersDropped = true
				point.TerminalSchemaSHA256, err = mysqlOptionCurrentSchemaDigest(
					db, "options",
				)
				require.NoError(t, err)
			} else {
				currentState, currentHasState, stateErr := inspectMySQLOptionMigrationState(
					db, "options",
				)
				require.NoError(t, stateErr)
				require.True(t, currentHasState)
				point.StatePhase = currentState.Phase
				currentTriggers, triggerErr := inspectMySQLOptionMigrationTriggerCandidates(
					db, artifacts,
				)
				require.NoError(t, triggerErr)
				if driftBlocked {
					point.BridgePhase = initialState.Phase
				} else {
					currentNames := make([]string, len(currentTriggers))
					for i, trigger := range currentTriggers {
						currentNames[i] = trigger.Name
					}
					slices.Sort(currentNames)
					for _, phase := range []string{
						optionMigrationPrepared,
						optionMigrationInsertGone,
						optionMigrationUpdateGone,
						optionMigrationBridgesGone,
					} {
						expectedNames, valid := mysqlOptionMigrationBridgeNamesForPhase(
							artifacts, phase,
						)
						slices.Sort(expectedNames)
						if valid && slices.Equal(currentNames, expectedNames) {
							point.BridgePhase = phase
							break
						}
					}
					require.NotEmpty(t, point.BridgePhase)
				}
			}
			restoreErr := restoreMySQLOptionFinalizationState(
				db, artifacts, initialState, retained, point,
			)
			joined := errors.Join(originalErr, restoreErr)

			if driftBlocked {
				require.ErrorIs(t, joined, errMySQLOptionFinalizationRecoveryBlocked)
				assert.Zero(t, recoveryDDL.Load())
				assert.Equal(t, beforeRestore,
					captureMySQLOptionRecoverySnapshot(t, db, artifacts.BackupTable))
				return
			}
			require.True(t, capabilityLost.Load(), "the target recovery guard must be exercised")
			require.ErrorIs(t, joined, originalErr)
			require.ErrorIs(t, joined, errMySQLMetadataVisibilityInspection)
			assert.ErrorContains(t, joined, "options migration recovery blocked")
			assert.NotContains(t, joined.Error(), privateErr.Error())
			assert.Equal(t, ddlAtCapabilityLoss.Load(), recoveryDDL.Load(),
				"capability loss must block every subsequent recovery DDL")
			if test.targetGuard == 1 {
				assert.Equal(t, beforeRestore,
					captureMySQLOptionRecoverySnapshot(t, db, artifacts.BackupTable))
			}
			afterFailure := captureMySQLOptionRecoverySnapshot(t, db, artifacts.BackupTable)
			ddlBeforeRestart := recoveryDDL.Load()

			restartErr := migrateOptionPrimaryKey(db.Session(&gorm.Session{NewDB: true}))

			if markersDropped && test.targetGuard == 1 {
				require.NoError(t, restartErr)
			} else {
				require.ErrorIs(t, restartErr, errMySQLMetadataVisibilityInspection)
			}
			assert.Equal(t, ddlBeforeRestart, recoveryDDL.Load(),
				"a second restart must not execute recovery DDL")
			assert.Equal(t, afterFailure,
				captureMySQLOptionRecoverySnapshot(t, db, artifacts.BackupTable))
		})
	}
}

func TestOptionPrimaryKeyMigrationHoldsLockThroughTerminalValidation(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 terminal validation lock regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options ("+
			"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
			"`value` longtext"+
			") ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
		"terminal.lock", "before",
	).Error)

	barrier := &optionMigrationTerminalValidationBarrier{
		Interface:         logger.Default.LogMode(logger.Silent),
		reached:           make(chan struct{}),
		continueMigration: make(chan struct{}),
	}
	migrationDone := make(chan error, 1)
	go func() {
		migrationDone <- migrateOptionPrimaryKey(
			db.Session(&gorm.Session{Logger: barrier, NewDB: true}),
		)
	}()
	select {
	case <-barrier.reached:
	case err := <-migrationDone:
		t.Fatalf("migration failed before terminal validation: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("migration did not reach terminal validation")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	writerConnectionID := make(chan int64, 1)
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- db.WithContext(ctx).Connection(func(writer *gorm.DB) error {
			var connectionID int64
			if err := writer.Raw("SELECT CONNECTION_ID()").Scan(&connectionID).Error; err != nil {
				return err
			}
			writerConnectionID <- connectionID
			return writer.Exec(
				"UPDATE options SET `value` = ? WHERE `key` = ?",
				"after", "terminal.lock",
			).Error
		})
	}()
	connectionID := <-writerConnectionID
	require.Eventually(t, func() bool {
		var blockedQueries int64
		err := db.Raw(`
SELECT count(*)
FROM information_schema.processlist
WHERE id = ? AND command <> 'Sleep' AND state IS NOT NULL`,
			connectionID).Scan(&blockedQueries).Error
		return err == nil && blockedQueries == 1
	}, 5*time.Second, 10*time.Millisecond, "writer must remain blocked through terminal validation")
	select {
	case err := <-writerDone:
		t.Fatalf("writer completed before the outer connection cleanup: %v", err)
	default:
	}

	close(barrier.continueMigration)
	require.NoError(t, <-migrationDone)
	require.NoError(t, <-writerDone)
	var value string
	require.NoError(t, db.Table("options").Select("`value`").
		Where("`key` = ?", "terminal.lock").Scan(&value).Error)
	assert.Equal(t, "after", value)
	assertNoMySQLOptionMigrationArtifacts(t, db)
}

func TestOptionPrimaryKeyMigrationRejectsCrossSchemaMySQLIncomingForeignKeys(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 cross-schema incoming foreign key regression")
	}
	t.Run("preflight", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(
			"CREATE TABLE options ("+
				"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
				"`value` longtext,"+
				"UNIQUE KEY `uniq_options_key` (`key`)"+
				") ENGINE=InnoDB",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
			"keep.option", "preserved",
		).Error)
		externalDefinition := createMySQLCrossSchemaOptionReference(t, db, "preflight", "options")
		var currentSchema string
		require.NoError(t, db.Raw("SELECT DATABASE()").Scan(&currentSchema).Error)
		targetSchema, targetTable := mysqlCrossSchemaOptionReferenceTarget(t, db, "preflight")
		require.Equal(t, currentSchema, targetSchema)
		require.Equal(t, "options", targetTable)

		err := migrateOptionPrimaryKey(db)

		require.ErrorContains(t, err, "MySQL options schema cannot be preserved safely")
		assert.Equal(t, externalDefinition, captureMySQLCrossSchemaOptionReference(t, db, "preflight"))
		assert.False(t, db.Migrator().HasColumn("options", optionMigrationMarkerCol))
		assertNoMySQLOptionMigrationArtifacts(t, db)
	})

	t.Run("post-snapshot and restart", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(
			"CREATE TABLE options ("+
				"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
				"`value` longtext,"+
				"UNIQUE KEY `uniq_options_key` (`key`)"+
				") ENGINE=InnoDB",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
			"keep.option", "preserved",
		).Error)

		barrier := &optionMigrationSnapshotBarrier{
			Interface:         logger.Default.LogMode(logger.Silent),
			statement:         "SELECT count(*) FROM `options_pk_tmp_",
			reached:           make(chan struct{}),
			continueMigration: make(chan struct{}),
		}
		migrationDone := make(chan error, 1)
		go func() {
			migrationDone <- migrateOptionPrimaryKey(
				db.Session(&gorm.Session{Logger: barrier, NewDB: true}),
			)
		}()
		select {
		case <-barrier.reached:
		case err := <-migrationDone:
			t.Fatalf("migration failed before the locked snapshot barrier: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("migration did not reach the locked snapshot barrier")
		}

		func() {
			defer close(barrier.continueMigration)
			createMySQLCrossSchemaOptionReference(t, db, "snapshot", "options")
			var incomingCount int64
			require.NoError(t, db.Raw(`
SELECT count(*)
FROM information_schema.key_column_usage
WHERE referenced_table_schema = DATABASE()
  AND referenced_table_name = 'options'
  AND constraint_schema <> DATABASE()`).Scan(&incomingCount).Error)
			require.EqualValues(t, 1, incomingCount)
		}()

		require.ErrorContains(t, <-migrationDone, "MySQL options schema changed during migration")
		var currentSchema string
		require.NoError(t, db.Raw("SELECT DATABASE()").Scan(&currentSchema).Error)
		targetSchema, retained := mysqlCrossSchemaOptionReferenceTarget(t, db, "snapshot")
		require.Equal(t, currentSchema, targetSchema)
		require.True(t, isOptionLegacyTable(retained))
		require.True(t, db.Migrator().HasTable("options"))
		require.True(t, db.Migrator().HasTable(retained))
		require.True(t, db.Migrator().HasColumn("options", optionMigrationMarkerCol))
		require.True(t, db.Migrator().HasColumn("options", optionMigrationStateCol))
		beforeRestart := captureMySQLOptionRecoverySnapshot(t, db, retained)
		externalDefinition := captureMySQLCrossSchemaOptionReference(t, db, "snapshot")

		for range 2 {
			require.ErrorContains(t,
				migrateOptionPrimaryKey(db.Session(&gorm.Session{NewDB: true})),
				"MySQL options schema changed during migration",
			)
			assert.Equal(t, beforeRestart, captureMySQLOptionRecoverySnapshot(t, db, retained))
			assert.Equal(t, externalDefinition, captureMySQLCrossSchemaOptionReference(t, db, "snapshot"))
			currentTargetSchema, currentRetained := mysqlCrossSchemaOptionReferenceTarget(t, db, "snapshot")
			assert.Equal(t, currentSchema, currentTargetSchema)
			assert.Equal(t, retained, currentRetained)
		}
	})

	t.Run("catalog query error fails closed", func(t *testing.T) {
		db := useMigrationTestDB(t)
		injected := errors.New("injected information schema failure")
		var failed atomic.Bool
		const callbackName = "test:mysql-incoming-fk-catalog-failure"
		require.NoError(t, db.Callback().Row().Before("gorm:row").Register(callbackName, func(tx *gorm.DB) {
			if strings.Contains(tx.Statement.SQL.String(), "AS has_incoming_foreign_key") &&
				failed.CompareAndSwap(false, true) {
				tx.AddError(injected)
			}
		}))
		t.Cleanup(func() {
			require.NoError(t, db.Callback().Row().Remove(callbackName))
		})

		err := validateMySQLOptionIncomingForeignKeys(db, "options")

		require.True(t, failed.Load(), "the catalog query must be exercised")
		require.ErrorContains(t, err, "inspect MySQL options schema metadata")
		require.ErrorIs(t, err, injected)
	})
}

func TestOptionPrimaryKeyMigrationRejectsMySQLIncomingForeignKeyAfterSnapshot(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 post-snapshot incoming foreign key regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options ("+
			"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
			"`value` longtext,"+
			"UNIQUE KEY `uniq_options_key` (`key`)"+
			") ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
		"keep.option", "preserved",
	).Error)

	barrier := &optionMigrationSnapshotBarrier{
		Interface:         logger.Default.LogMode(logger.Silent),
		statement:         "SELECT count(*) FROM `options_pk_tmp_",
		reached:           make(chan struct{}),
		continueMigration: make(chan struct{}),
	}
	migrationDone := make(chan error, 1)
	go func() {
		migrationDone <- migrateOptionPrimaryKey(
			db.Session(&gorm.Session{Logger: barrier, NewDB: true}),
		)
	}()
	select {
	case <-barrier.reached:
	case err := <-migrationDone:
		t.Fatalf("migration failed before the locked snapshot barrier: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("migration did not reach the locked snapshot barrier")
	}

	require.NoError(t, db.Exec(
		"CREATE TABLE options_snapshot_reference ("+
			"`id` bigint PRIMARY KEY, "+
			"`option_key` varchar(191) CHARACTER SET utf8mb4 "+
			"COLLATE utf8mb4_unicode_ci NOT NULL, "+
			"CONSTRAINT fk_options_snapshot_reference "+
			"FOREIGN KEY (`option_key`) REFERENCES options (`key`)"+
			") ENGINE=InnoDB",
	).Error, "MySQL 5.7 must reproduce incoming FK creation while the snapshot lock is held")
	require.NoError(t, db.Exec(
		"INSERT INTO options_snapshot_reference (`id`, `option_key`) VALUES (1, ?)",
		"keep.option",
	).Error)
	close(barrier.continueMigration)

	require.ErrorContains(t, <-migrationDone, "MySQL options schema changed during migration")
	var retained string
	require.NoError(t, db.Raw(`
SELECT referenced_table_name
FROM information_schema.key_column_usage
WHERE constraint_schema = DATABASE()
  AND constraint_name = 'fk_options_snapshot_reference'`,
	).Scan(&retained).Error)
	require.True(t, isOptionLegacyTable(retained))
	require.True(t, db.Migrator().HasTable("options"))
	require.True(t, db.Migrator().HasTable(retained))
	require.True(t, db.Migrator().HasColumn("options", optionMigrationMarkerCol))
	require.True(t, db.Migrator().HasColumn("options", optionMigrationStateCol))
	beforeRestart := captureMySQLOptionRecoverySnapshot(t, db, retained)
	externalDDL, err := showCreateMySQLTable(db, "options_snapshot_reference")
	require.NoError(t, err)

	for range 2 {
		require.ErrorContains(t,
			migrateOptionPrimaryKey(db.Session(&gorm.Session{NewDB: true})),
			"MySQL options schema changed during migration",
		)
		assert.Equal(t, beforeRestart, captureMySQLOptionRecoverySnapshot(t, db, retained))
		currentExternalDDL, err := showCreateMySQLTable(db, "options_snapshot_reference")
		require.NoError(t, err)
		assert.Equal(t, externalDDL, currentExternalDDL)
		var currentRetained string
		require.NoError(t, db.Raw(`
SELECT referenced_table_name
FROM information_schema.key_column_usage
WHERE constraint_schema = DATABASE()
  AND constraint_name = 'fk_options_snapshot_reference'`,
		).Scan(&currentRetained).Error)
		assert.Equal(t, retained, currentRetained)
	}
}

func TestMySQLOptionMigrationWriteBarrierBridgesAtomicRename(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 table-lock protocol")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (`key` varchar(191) NOT NULL, `value` longtext) ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
		"concurrent.option", "snapshot",
	).Error)
	require.NoError(t, db.Exec("CREATE TABLE options_pk_tmp LIKE options").Error)
	require.NoError(t, db.Exec("ALTER TABLE options_pk_tmp ADD PRIMARY KEY (`key`)").Error)
	require.NoError(t, db.Exec(
		"CREATE TRIGGER options_pk_probe_au AFTER UPDATE ON options FOR EACH ROW "+
			"INSERT INTO options_pk_tmp (`key`, `value`) VALUES (NEW.`key`, NEW.`value`) "+
			"ON DUPLICATE KEY UPDATE `value` = NEW.`value`",
	).Error)
	t.Cleanup(func() { _ = db.Exec("DROP TRIGGER IF EXISTS options_pk_probe_au").Error })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	writerConnectionID := make(chan int64, 1)
	writerDone := make(chan error, 1)
	require.NoError(t, db.WithContext(ctx).Connection(func(locked *gorm.DB) error {
		if err := locked.Exec("LOCK TABLES options WRITE, options_pk_tmp WRITE").Error; err != nil {
			return err
		}

		go func() {
			writerDone <- db.WithContext(ctx).Connection(func(writer *gorm.DB) error {
				var connectionID int64
				if err := writer.Raw("SELECT CONNECTION_ID()").Scan(&connectionID).Error; err != nil {
					return err
				}
				writerConnectionID <- connectionID
				return writer.Exec(
					"UPDATE options SET `value` = ? WHERE `key` = ?",
					"writer", "concurrent.option",
				).Error
			})
		}()
		connectionID := <-writerConnectionID
		require.Eventually(t, func() bool {
			var blockedQueries int64
			err := db.Raw(`
SELECT count(*)
FROM information_schema.processlist
WHERE id = ? AND command <> 'Sleep' AND state IS NOT NULL`,
				connectionID).Scan(&blockedQueries).Error
			return err == nil && blockedQueries == 1
		}, 5*time.Second, 10*time.Millisecond, "server never observed the writer blocked on the table lock")
		select {
		case err := <-writerDone:
			return fmt.Errorf("writer completed while source table was write-locked: %w", err)
		default:
		}
		if err := locked.Exec(
			"INSERT INTO options_pk_tmp (`key`, `value`) SELECT `key`, `value` FROM options",
		).Error; err != nil {
			return err
		}
		if err := locked.Exec("UNLOCK TABLES").Error; err != nil {
			return err
		}
		return locked.Exec(
			"RENAME TABLE options TO options_legacy_probe, options_pk_tmp TO options",
		).Error
	}))
	require.NoError(t, <-writerDone)
	require.NoError(t, db.Exec("DROP TRIGGER options_pk_probe_au").Error)

	var current Option
	require.NoError(t, db.Where("`key` = ?", "concurrent.option").First(&current).Error)
	assert.True(t, current.Value == "writer", "writer must resume against the replacement table")
}

func TestOptionPrimaryKeyMigrationBridgesMySQLWritesAfterUnlockBeforeRename(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 post-unlock bridge regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options ("+
			"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
			"`value` longtext"+
			") ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES "+
			"(?, ?), (?, ?), (?, ?), (?, ?), (?, ?)",
		"delete.option", "delete-me",
		"update.option", "rename-me",
		"same.option", "before",
		"Alias.Option", "alias-value",
		"alias.option", "alias-value",
	).Error)

	barrier := &optionMigrationSnapshotBarrier{
		Interface:         logger.Default.LogMode(logger.Silent),
		statement:         "UNLOCK TABLES",
		reached:           make(chan struct{}),
		continueMigration: make(chan struct{}),
	}
	migrationDone := make(chan error, 1)
	go func() {
		migrationDone <- migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: barrier}))
	}()
	select {
	case <-barrier.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("migration did not reach the post-unlock barrier")
	}

	require.NoError(t, db.Exec(
		"DELETE FROM options WHERE OCTET_LENGTH(`key`) = ? AND BINARY `key` = BINARY ?",
		len("delete.option"), "delete.option",
	).Error)
	require.NoError(t, db.Exec(
		"UPDATE options SET `key` = ?, `value` = ? "+
			"WHERE OCTET_LENGTH(`key`) = ? AND BINARY `key` = BINARY ?",
		"renamed.option", "renamed", len("update.option"), "update.option",
	).Error)
	require.NoError(t, db.Exec(
		"UPDATE options SET `value` = ? WHERE `key` = ?",
		"after", "same.option",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
		"insert.option", "inserted",
	).Error)
	require.NoError(t, db.Exec(
		"DELETE FROM options WHERE OCTET_LENGTH(`key`) = ? AND BINARY `key` = BINARY ?",
		len("alias.option"), "alias.option",
	).Error)
	require.NoError(t, db.Exec(
		"UPDATE options SET `key` = ? WHERE OCTET_LENGTH(`key`) = ? AND BINARY `key` = BINARY ?",
		"ALIAS.OPTION", len("Alias.Option"), "Alias.Option",
	).Error)
	close(barrier.continueMigration)
	require.NoError(t, <-migrationDone)

	var options []Option
	require.NoError(t, db.Order(clause.OrderByColumn{Column: clause.Column{Name: "key"}}).Find(&options).Error)
	assert.Equal(t, []Option{
		{Key: "Alias.Option", Value: "alias-value"},
		{Key: "insert.option", Value: "inserted"},
		{Key: "renamed.option", Value: "renamed"},
		{Key: "same.option", Value: "after"},
	}, options)
	assertNoMySQLOptionMigrationArtifacts(t, db)
}

func TestOptionPrimaryKeyMigrationBridgesMySQLAliasRenameAfterUnlockBeforeRename(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 post-unlock alias rename bridge regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options ("+
			"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
			"`value` longtext"+
			") ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (?, ?), (?, ?)",
		"Alias.Option", "same", "alias.option", "same",
	).Error)

	barrier := &optionMigrationSnapshotBarrier{
		Interface:         logger.Default.LogMode(logger.Silent),
		statement:         "UNLOCK TABLES",
		reached:           make(chan struct{}),
		continueMigration: make(chan struct{}),
	}
	migrationDone := make(chan error, 1)
	go func() {
		migrationDone <- migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: barrier}))
	}()
	select {
	case <-barrier.reached:
	case err := <-migrationDone:
		t.Fatalf("migration failed before the post-unlock barrier: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("migration did not reach the post-unlock barrier")
	}

	require.NoError(t, db.Exec(
		"DELETE FROM options WHERE OCTET_LENGTH(`key`) = ? AND BINARY `key` = BINARY ?",
		len("alias.option"), "alias.option",
	).Error)
	require.NoError(t, db.Exec(
		"UPDATE options SET `key` = ? "+
			"WHERE OCTET_LENGTH(`key`) = ? AND BINARY `key` = BINARY ?",
		"ALIAS.OPTION", len("Alias.Option"), "Alias.Option",
	).Error)
	close(barrier.continueMigration)
	require.NoError(t, <-migrationDone)

	var options []Option
	require.NoError(t, db.Find(&options).Error)
	assert.Equal(t, []Option{{Key: "Alias.Option", Value: "same"}}, options)
	assertNoMySQLOptionMigrationArtifacts(t, db)
}

func TestOptionPrimaryKeyMigrationRejectsMySQLAliasValueConflictsAfterUnlock(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 post-unlock alias conflict regression")
	}
	for _, test := range []struct {
		name        string
		initialRows []struct {
			key   string
			value any
		}
		mutate          func(*gorm.DB) error
		wantSourceRows  int64
		wantSourceValue sql.NullString
		wantError       string
	}{
		{
			name: "update alias to a different value",
			initialRows: []struct {
				key   string
				value any
			}{
				{key: "Alias.Option", value: "same"},
				{key: "alias.option", value: "same"},
			},
			mutate: func(db *gorm.DB) error {
				return db.Exec(
					"UPDATE options SET `value` = ? "+
						"WHERE OCTET_LENGTH(`key`) = ? AND BINARY `key` = BINARY ?",
					"different", len("alias.option"), "alias.option",
				).Error
			},
			wantSourceRows:  2,
			wantSourceValue: sql.NullString{String: "same", Valid: true},
			wantError:       "options migration value conflict",
		},
		{
			name: "insert null alias beside empty value",
			initialRows: []struct {
				key   string
				value any
			}{
				{key: "Alias.Option", value: ""},
			},
			mutate: func(db *gorm.DB) error {
				return db.Exec(
					"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
					"alias.option", nil,
				).Error
			},
			wantSourceRows:  1,
			wantSourceValue: sql.NullString{String: "", Valid: true},
			wantError:       "options migration null key or value",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options ("+
					"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
					"`value` longtext"+
					") ENGINE=InnoDB",
			).Error)
			for _, row := range test.initialRows {
				require.NoError(t, db.Exec(
					"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
					row.key, row.value,
				).Error)
			}

			barrier := &optionMigrationSnapshotBarrier{
				Interface:         logger.Default.LogMode(logger.Silent),
				statement:         "UNLOCK TABLES",
				reached:           make(chan struct{}),
				continueMigration: make(chan struct{}),
			}
			migrationDone := make(chan error, 1)
			go func() {
				migrationDone <- migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: barrier}))
			}()
			select {
			case <-barrier.reached:
			case err := <-migrationDone:
				t.Fatalf("migration failed before the post-unlock barrier: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("migration did not reach the post-unlock barrier")
			}

			writerErr := test.mutate(db)
			var sourceRows int64
			sourceReadErr := db.Table("options").Where("`key` = ?", "Alias.Option").Count(&sourceRows).Error
			var sourceValues []struct {
				Value sql.NullString `gorm:"column:value"`
			}
			sourceValuesErr := db.Table("options").Select("`value`").
				Where("`key` = ?", "Alias.Option").Find(&sourceValues).Error
			var tmpValue sql.NullString
			tmpTable := ownedMySQLOptionMigrationTempTable(t, db)
			tmpReadErr := db.Table(tmpTable).Select("`value`").
				Where("`key` = ?", "Alias.Option").Scan(&tmpValue).Error
			close(barrier.continueMigration)
			migrationErr := <-migrationDone

			require.ErrorContains(t, writerErr, test.wantError)
			require.NoError(t, sourceReadErr)
			require.NoError(t, sourceValuesErr)
			require.NoError(t, tmpReadErr)
			assert.Equal(t, test.wantSourceRows, sourceRows)
			require.Len(t, sourceValues, int(test.wantSourceRows))
			for _, row := range sourceValues {
				assert.Equal(t, test.wantSourceValue, row.Value)
			}
			assert.Equal(t, test.wantSourceValue, tmpValue)
			require.NoError(t, migrationErr)

			var finalValue sql.NullString
			require.NoError(t, db.Table("options").Select("`value`").
				Where("`key` = ?", "Alias.Option").Scan(&finalValue).Error)
			assert.Equal(t, test.wantSourceValue, finalValue)
			assertNoMySQLOptionMigrationArtifacts(t, db)
		})
	}
}

func TestOptionPrimaryKeyMigrationRejectsMySQLNullWritesAfterUnlock(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 post-unlock NULL write regression")
	}
	for _, test := range []struct {
		name   string
		mutate func(*gorm.DB) error
	}{
		{
			name: "insert new key with null value",
			mutate: func(db *gorm.DB) error {
				return db.Exec(
					"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
					"insert.null.option", nil,
				).Error
			},
		},
		{
			name: "update existing value to null",
			mutate: func(db *gorm.DB) error {
				return db.Exec(
					"UPDATE options SET `value` = ? WHERE `key` = ?",
					nil, "stable.option",
				).Error
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options ("+
					"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci,"+
					"`value` longtext NULL"+
					") ENGINE=InnoDB",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
				"stable.option", "preserved",
			).Error)

			barrier := &optionMigrationSnapshotBarrier{
				Interface:         logger.Default.LogMode(logger.Silent),
				statement:         "UNLOCK TABLES",
				reached:           make(chan struct{}),
				continueMigration: make(chan struct{}),
			}
			migrationDone := make(chan error, 1)
			go func() {
				migrationDone <- migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: barrier}))
			}()
			select {
			case <-barrier.reached:
			case err := <-migrationDone:
				t.Fatalf("migration failed before the post-unlock barrier: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("migration did not reach the post-unlock barrier")
			}

			tmpTable := ownedMySQLOptionMigrationTempTable(t, db)
			writerErr := test.mutate(db)
			var sourceNulls int64
			sourceReadErr := db.Table("options").
				Where("`key` IS NULL OR `value` IS NULL").Count(&sourceNulls).Error
			var tmpNulls int64
			tmpReadErr := db.Table(tmpTable).
				Where("`key` IS NULL OR `value` IS NULL").Count(&tmpNulls).Error
			close(barrier.continueMigration)
			migrationErr := <-migrationDone

			require.ErrorContains(t, writerErr, "options migration null key or value")
			assert.NotContains(t, writerErr.Error(), "insert.null.option")
			assert.NotContains(t, writerErr.Error(), "stable.option")
			require.NoError(t, sourceReadErr)
			assert.Zero(t, sourceNulls)
			require.NoError(t, tmpReadErr)
			assert.Zero(t, tmpNulls)
			require.NoError(t, migrationErr)
			var finalNulls int64
			require.NoError(t, db.Table("options").
				Where("`key` IS NULL OR `value` IS NULL").Count(&finalNulls).Error)
			assert.Zero(t, finalNulls)
			var options []Option
			require.NoError(t, db.Find(&options).Error)
			assert.Equal(t, []Option{{Key: "stable.option", Value: "preserved"}}, options)
			assertNoMySQLOptionMigrationArtifacts(t, db)
		})
	}
}

func TestOptionPrimaryKeyMigrationCleansMySQLArtifactsAfterCancellation(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 cancellation cleanup regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (`key` varchar(191) NOT NULL, `value` longtext) ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
		"cancel.option", "preserved",
	).Error)

	ctx, cancel := context.WithCancel(t.Context())
	barrier := &optionMigrationSnapshotBarrier{
		Interface:         logger.Default.LogMode(logger.Silent),
		statement:         "CREATE TRIGGER `" + optionUpdateTriggerPrefix,
		reached:           make(chan struct{}),
		continueMigration: make(chan struct{}),
	}
	migrationDone := make(chan error, 1)
	go func() {
		migrationDone <- migrateOptionPrimaryKey(
			db.WithContext(ctx).Session(&gorm.Session{Logger: barrier}),
		)
	}()
	select {
	case <-barrier.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("migration did not reach the post-trigger barrier")
	}
	cancel()
	close(barrier.continueMigration)
	require.Error(t, <-migrationDone)
	assertNoMySQLOptionMigrationArtifacts(t, db)

	var option Option
	require.NoError(t, db.Where("`key` = ?", "cancel.option").First(&option).Error)
	assert.Equal(t, "preserved", option.Value)
}

func TestOptionPrimaryKeyMigrationInstallsMySQLTriggersInSafeOrder(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 trigger-install ordering regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (`key` varchar(191) NOT NULL, `value` longtext) ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
		"stable.option", "preserved",
	).Error)

	barrier := &optionMigrationSnapshotBarrier{
		Interface:         logger.Default.LogMode(logger.Silent),
		statement:         "CREATE TRIGGER `" + optionUpdateTriggerPrefix,
		reached:           make(chan struct{}),
		continueMigration: make(chan struct{}),
	}
	migrationDone := make(chan error, 1)
	go func() {
		migrationDone <- migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: barrier}))
	}()
	select {
	case <-barrier.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("migration did not reach the trigger-install barrier")
	}
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
		"transient.option", "transient",
	).Error)
	require.NoError(t, db.Exec(
		"DELETE FROM options WHERE `key` = ?",
		"transient.option",
	).Error)
	close(barrier.continueMigration)
	require.NoError(t, <-migrationDone)

	var options []Option
	require.NoError(t, db.Find(&options).Error)
	assert.Equal(t, []Option{{Key: "stable.option", Value: "preserved"}}, options)
	assertNoMySQLOptionMigrationArtifacts(t, db)
}

func assertNoMySQLOptionMigrationArtifacts(t *testing.T, db *gorm.DB) {
	t.Helper()
	if db.Dialector.Name() != "mysql" {
		return
	}
	var markerCount int64
	require.NoError(t, db.Raw(`
SELECT count(*)
FROM information_schema.columns
WHERE table_schema = DATABASE()
  AND ((column_name = ? AND (column_comment = ? OR column_comment LIKE ?))
    OR (column_name = ? AND column_comment = ?))`,
		optionMigrationMarkerCol, optionMigrationMarker, optionMigrationMarker+"/%",
		optionMigrationStateCol, optionMigrationStateMarker,
	).Scan(&markerCount).Error)
	assert.Zero(t, markerCount)
	var triggerCount int64
	require.NoError(t, db.Raw(`
SELECT count(*)
FROM information_schema.triggers
WHERE trigger_schema = DATABASE() AND action_statement LIKE ?`,
		"%"+optionMigrationMarker+"%",
	).Scan(&triggerCount).Error)
	assert.Zero(t, triggerCount)
	assert.False(t, mysqlOptionTerminalJournalExists(t, db))
	var lockFree int
	require.NoError(t, db.Raw("SELECT IS_FREE_LOCK(?)", optionPrimaryKeyLockName).Scan(&lockFree).Error)
	assert.Equal(t, 1, lockFree)
}

func ownedMySQLOptionMigrationTempTable(t *testing.T, db *gorm.DB) string {
	t.Helper()
	var table string
	require.NoError(t, db.Raw(`
SELECT table_name
FROM information_schema.columns
WHERE table_schema = DATABASE()
  AND column_name = ?
  AND column_comment LIKE ?
  AND table_name LIKE ?
LIMIT 1`,
		optionMigrationMarkerCol, optionMigrationMarker+"/%", optionMySQLTmpPrefix+"%",
	).Scan(&table).Error)
	require.NotEmpty(t, table)
	return table
}

func TestOptionPrimaryKeyMigrationBlocksMySQLWriterAcrossSnapshotAndSwap(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 online-writer regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (`key` varchar(191) NOT NULL, `value` longtext) ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
		"concurrent.option", "snapshot",
	).Error)
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = map[string]string{}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})

	barrier := &optionMigrationSnapshotBarrier{
		Interface:         logger.Default.LogMode(logger.Silent),
		statement:         "LOCK TABLES `options` WRITE, `" + optionMySQLTmpPrefix,
		reached:           make(chan struct{}),
		continueMigration: make(chan struct{}),
	}
	migrationDone := make(chan error, 1)
	go func() {
		migrationDone <- migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: barrier}))
	}()
	select {
	case <-barrier.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("migration did not reach the post-snapshot barrier")
	}

	writerConnectionID := make(chan int64, 1)
	var writerQueryObserved atomic.Bool
	const callbackName = "test:options-migration-blocked-writer"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table != "options" || !writerQueryObserved.CompareAndSwap(false, true) {
			return
		}
		ctx := tx.Statement.Context
		if ctx == nil {
			ctx = context.Background()
		}
		var connectionID int64
		if err := tx.Statement.ConnPool.QueryRowContext(ctx, "SELECT CONNECTION_ID()").
			Scan(&connectionID); err != nil {
			tx.AddError(err)
			return
		}
		writerConnectionID <- connectionID
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Query().Remove(callbackName))
	})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- UpdateOption("concurrent.option", "writer")
	}()
	connectionID := <-writerConnectionID
	require.Eventually(t, func() bool {
		var blockedQueries int64
		err := db.Raw(`
SELECT count(*)
FROM information_schema.processlist
WHERE id = ? AND command <> 'Sleep' AND state IS NOT NULL`,
			connectionID).Scan(&blockedQueries).Error
		return err == nil && blockedQueries == 1
	}, 5*time.Second, 10*time.Millisecond, "server never observed the option writer blocked on the table lock")
	select {
	case err := <-writerDone:
		t.Fatalf("writer completed while migration held the table lock: %v", err)
	default:
	}
	close(barrier.continueMigration)
	require.NoError(t, <-migrationDone)
	require.NoError(t, <-writerDone)

	var current Option
	require.NoError(t, db.Where("`key` = ?", "concurrent.option").First(&current).Error)
	assert.True(t, current.Value == "writer", "the post-swap writer value must remain authoritative")
	assertNoMySQLOptionMigrationArtifacts(t, db)
}

func TestOptionPrimaryKeyMigrationAcceptsMySQLWriterBeforeSnapshotLock(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 online-writer pre-lock regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (`key` varchar(191) NOT NULL, `value` longtext) ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
		"concurrent.option", "snapshot",
	).Error)
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = map[string]string{}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})

	barrier := &optionMigrationSnapshotBarrier{
		Interface:         logger.Default.LogMode(logger.Silent),
		statement:         "CREATE TRIGGER `" + optionUpdateTriggerPrefix,
		reached:           make(chan struct{}),
		continueMigration: make(chan struct{}),
	}
	migrationDone := make(chan error, 1)
	go func() {
		migrationDone <- migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: barrier}))
	}()
	select {
	case <-barrier.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("migration did not reach the post-trigger barrier")
	}
	require.NoError(t, UpdateOption("concurrent.option", "writer"))
	close(barrier.continueMigration)
	require.NoError(t, <-migrationDone)

	var current Option
	require.NoError(t, db.Where("`key` = ?", "concurrent.option").First(&current).Error)
	assert.True(t, current.Value == "writer", "the pre-lock writer value must survive the swap")
	assertNoMySQLOptionMigrationArtifacts(t, db)
}

func TestOptionPrimaryKeyMigrationRejectsConcurrentMySQLDDLBeforeSnapshotLock(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 concurrent-DDL regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (`key` varchar(191) NOT NULL, `value` longtext) ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
		"stable.option", "preserved",
	).Error)

	barrier := &optionMigrationSnapshotBarrier{
		Interface:         logger.Default.LogMode(logger.Silent),
		statement:         "CREATE TRIGGER `" + optionInsertTriggerPrefix,
		reached:           make(chan struct{}),
		continueMigration: make(chan struct{}),
	}
	migrationDone := make(chan error, 1)
	go func() {
		migrationDone <- migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: barrier}))
	}()
	select {
	case <-barrier.reached:
	case err := <-migrationDone:
		t.Fatalf("migration failed before the concurrent-DDL barrier: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("migration did not reach the concurrent-DDL barrier")
	}

	ddlErr := db.Exec(
		"ALTER TABLE options ADD COLUMN concurrent_marker varchar(32) NOT NULL DEFAULT 'preserved-marker'",
	).Error
	close(barrier.continueMigration)
	migrationErr := <-migrationDone

	require.NoError(t, ddlErr)
	if assert.Error(t, migrationErr) {
		assert.Contains(t, migrationErr.Error(), "options schema changed during migration")
		assert.NotContains(t, migrationErr.Error(), "preserved")
	}
	assert.True(t, db.Migrator().HasColumn("options", "concurrent_marker"),
		"the concurrent schema must remain on the active source table")
	var row struct {
		Value  string
		Marker string `gorm:"column:concurrent_marker"`
	}
	if assert.NoError(t, db.Table("options").Select("`value`, `concurrent_marker`").
		Where("`key` = ?", "stable.option").Scan(&row).Error) {
		assert.Equal(t, "preserved", row.Value)
		assert.Equal(t, "preserved-marker", row.Marker)
	}
	assertNoMySQLOptionMigrationArtifacts(t, db)
}

func TestOptionPrimaryKeyMigrationPreservesBothTablesAfterConcurrentMySQLTriggerDDL(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 post-unlock concurrent-DDL regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (`key` varchar(191) NOT NULL, `value` longtext) ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
		"stable.option", "preserved",
	).Error)

	barrier := &optionMigrationSnapshotBarrier{
		Interface:         logger.Default.LogMode(logger.Silent),
		statement:         "UNLOCK TABLES",
		reached:           make(chan struct{}),
		continueMigration: make(chan struct{}),
	}
	migrationDone := make(chan error, 1)
	go func() {
		migrationDone <- migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: barrier}))
	}()
	select {
	case <-barrier.reached:
	case err := <-migrationDone:
		t.Fatalf("migration failed before the post-unlock DDL barrier: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("migration did not reach the post-unlock DDL barrier")
	}

	triggerErr := db.Exec(
		"CREATE TRIGGER options_concurrent_bi BEFORE INSERT ON options " +
			"FOR EACH ROW SET NEW.`value` = CONCAT(NEW.`value`, '-triggered')",
	).Error
	close(barrier.continueMigration)
	migrationErr := <-migrationDone

	require.NoError(t, triggerErr)
	require.ErrorContains(t, migrationErr, "options schema changed during migration")
	assert.NotContains(t, migrationErr.Error(), "preserved")
	primary, primaryErr := optionsKeyIsPrimary(db)
	require.NoError(t, primaryErr)
	assert.True(t, primary, "post-swap drift must leave the replacement active")
	assert.True(t, db.Migrator().HasColumn("options", optionMigrationMarkerCol))
	assert.True(t, db.Migrator().HasColumn("options", optionMigrationStateCol))
	var backup string
	require.NoError(t, db.Raw(`
SELECT event_object_table
FROM information_schema.triggers
WHERE trigger_schema = DATABASE() AND trigger_name = 'options_concurrent_bi'`).
		Scan(&backup).Error)
	require.True(t, isOptionLegacyTable(backup))
	assert.True(t, db.Migrator().HasTable(backup))
	var triggerTable string
	require.NoError(t, db.Raw(`
SELECT event_object_table
FROM information_schema.triggers
WHERE trigger_schema = DATABASE() AND trigger_name = 'options_concurrent_bi'`).
		Scan(&triggerTable).Error)
	assert.Equal(t, backup, triggerTable, "unowned trigger must remain on the retained source")
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
		"trigger.option", "new",
	).Error)
	var value string
	require.NoError(t, db.Table("options").Select("`value`").
		Where("`key` = ?", "trigger.option").Scan(&value).Error)
	assert.Equal(t, "new", value)
	require.ErrorContains(t,
		migrateOptionPrimaryKey(db.Session(&gorm.Session{NewDB: true})),
		"options schema changed during migration",
	)
	var retainedValue string
	require.NoError(t, db.Table(backup).Select("`value`").
		Where("`key` = ?", "stable.option").Scan(&retainedValue).Error)
	assert.Equal(t, "preserved", retainedValue)
}

func TestOptionPrimaryKeyMigrationPreservesMySQLSourceCollation(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 source-collation regression")
	}
	for _, test := range []struct {
		name         string
		collation    string
		wantRowCount int64
	}{
		{name: "binary keeps distinct keys", collation: "utf8mb4_bin", wantRowCount: 2},
		{name: "unicode converges aliases", collation: "utf8mb4_unicode_ci", wantRowCount: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options ("+
					"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE "+test.collation+" NULL,"+
					"`value` longtext,"+
					"`schema_marker` varchar(32) NOT NULL DEFAULT 'preserved',"+
					"KEY `idx_options_value` (`value`(16))"+
					") ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES (?, ?), (?, ?)",
				"Foo", "same", "foo", "same",
			).Error)

			require.NoError(t, migrateOptionPrimaryKey(db))

			var rowCount int64
			require.NoError(t, db.Table("options").Count(&rowCount).Error)
			assert.Equal(t, test.wantRowCount, rowCount)
			var keyColumn struct {
				ColumnType   string `gorm:"column:COLUMN_TYPE"`
				IsNullable   string `gorm:"column:IS_NULLABLE"`
				CharacterSet string `gorm:"column:character_set"`
				Collation    string `gorm:"column:collation"`
			}
			require.NoError(t, db.Raw(`
SELECT COLUMN_TYPE, IS_NULLABLE, CHARACTER_SET_NAME AS character_set, COLLATION_NAME AS collation
FROM information_schema.columns
WHERE table_schema = DATABASE() AND table_name = 'options' AND column_name = 'key'`,
			).Scan(&keyColumn).Error)
			assert.Equal(t, "varchar(191)", keyColumn.ColumnType)
			assert.Equal(t, "NO", keyColumn.IsNullable)
			assert.Equal(t, "utf8mb4", keyColumn.CharacterSet)
			assert.Equal(t, test.collation, keyColumn.Collation)
			assert.True(t, db.Migrator().HasColumn("options", "schema_marker"))
			assert.True(t, db.Migrator().HasIndex("options", "idx_options_value"))
			assertNoMySQLOptionMigrationArtifacts(t, db)
		})
	}
}

func TestOptionPrimaryKeyMigrationPreservesMySQLBinaryTrailingSpaceKeys(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 binary trailing-space regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options ("+
			"`key` varbinary(191) NOT NULL,"+
			"`value` longtext"+
			") ENGINE=InnoDB",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (?, ?), (?, ?)",
		"foo", "same",
		"foo ", "same",
	).Error)

	require.NoError(t, migrateOptionPrimaryKey(db))

	primary, err := optionsKeyIsPrimary(db)
	require.NoError(t, err)
	assert.True(t, primary)
	var rows []struct {
		Key []byte `gorm:"column:key"`
	}
	require.NoError(t, db.Table("options").Select("`key`").Order("BINARY `key`").Find(&rows).Error)
	require.Len(t, rows, 2)
	assert.Equal(t, []byte("foo"), rows[0].Key)
	assert.Equal(t, []byte("foo "), rows[1].Key)
	assertNoMySQLOptionMigrationArtifacts(t, db)
}

func TestOptionPrimaryKeyMigrationRejectsMySQLDistinctExtendedColumnValues(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 extended-column deduplication regression")
	}
	for _, test := range []struct {
		name   string
		first  any
		second any
	}{
		{name: "distinct strings", first: "first-marker", second: "second-marker"},
		{name: "null and empty", first: nil, second: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options ("+
					"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
					"`value` longtext,"+
					"`schema_marker` varchar(32) NULL"+
					") ENGINE=InnoDB",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`, `schema_marker`) VALUES (?, ?, ?), (?, ?, ?)",
				"Foo", "same", test.first,
				"foo", "same", test.second,
			).Error)
			recorder := &migrationSQLRecorder{}

			err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

			require.ErrorContains(t, err, "extended columns cannot be deduplicated safely")
			assert.NotContains(t, err.Error(), "first-marker")
			assert.NotContains(t, err.Error(), "second-marker")
			assert.Empty(t, recorder.schemaMutations(), "conflict must fail before temporary DDL")
			var rows []struct {
				Key    string         `gorm:"column:key"`
				Marker sql.NullString `gorm:"column:schema_marker"`
			}
			require.NoError(t, db.Table("options").Select("`key`, `schema_marker`").
				Order("BINARY `key`").Find(&rows).Error)
			require.Len(t, rows, 2)
			assert.Equal(t, "Foo", rows[0].Key)
			assert.Equal(t, "foo", rows[1].Key)
			assert.Equal(t, sqlNullString(test.first), rows[0].Marker)
			assert.Equal(t, sqlNullString(test.second), rows[1].Marker)
			assertNoMySQLOptionMigrationArtifacts(t, db)
		})
	}
}

func sqlNullString(value any) sql.NullString {
	if value == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: value.(string), Valid: true}
}

func TestOptionPrimaryKeyMigrationFailsClosedOnMySQLUncopiedSchemaObjects(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 CREATE TABLE LIKE schema-loss regression")
	}
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *gorm.DB)
	}{
		{
			name: "foreign key",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(
					"CREATE TABLE option_values (`value` varchar(64) PRIMARY KEY) ENGINE=InnoDB",
				).Error)
				require.NoError(t, db.Exec(
					"INSERT INTO option_values (`value`) VALUES ('allowed')",
				).Error)
				require.NoError(t, db.Exec(
					"CREATE TABLE options ("+
						"`key` varchar(191) NOT NULL, `value` varchar(64),"+
						"CONSTRAINT fk_options_value FOREIGN KEY (`value`) REFERENCES option_values (`value`)"+
						") ENGINE=InnoDB",
				).Error)
			},
		},
		{
			name: "custom trigger",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(
					"CREATE TABLE options (`key` varchar(191) NOT NULL, `value` longtext) ENGINE=InnoDB",
				).Error)
				require.NoError(t, db.Exec(
					"CREATE TRIGGER options_custom_bi BEFORE INSERT ON options "+
						"FOR EACH ROW SET NEW.`value` = NEW.`value`",
				).Error)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			test.setup(t, db)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
				"schema.option", "allowed",
			).Error)

			err := migrateOptionPrimaryKey(db)
			require.ErrorContains(t, err, "MySQL options schema cannot be preserved safely")
			assert.NotContains(t, err.Error(), "allowed")
			assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
			var value string
			require.NoError(t, db.Table("options").Select("`value`").
				Where("`key` = ?", "schema.option").Scan(&value).Error)
			assert.Equal(t, "allowed", value)
			var schemaObjects int64
			if test.name == "foreign key" {
				require.NoError(t, db.Raw(`
SELECT count(*) FROM information_schema.key_column_usage
WHERE constraint_schema = DATABASE() AND table_name = 'options'
  AND referenced_table_name IS NOT NULL`).Scan(&schemaObjects).Error)
			} else {
				require.NoError(t, db.Raw(`
SELECT count(*) FROM information_schema.triggers
WHERE trigger_schema = DATABASE() AND event_object_table = 'options'`).Scan(&schemaObjects).Error)
			}
			assert.EqualValues(t, 1, schemaObjects)
		})
	}
}

func TestOptionPrimaryKeyMigrationFailsClosedOnSQLiteCustomCollation(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite custom-collation regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (`key` text COLLATE NOCASE, `value` text)",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (?, ?), (?, ?)",
		"Foo", "same", "foo", "same",
	).Error)

	err := migrateOptionPrimaryKey(db)
	require.ErrorContains(t, err, "SQLite options key collation cannot be preserved safely")
	var rowCount int64
	require.NoError(t, db.Table("options").Count(&rowCount).Error)
	assert.EqualValues(t, 2, rowCount)
	assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
}

func TestOptionPrimaryKeyMigrationPreservesPostgresExtendedSchemaRows(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "postgres" {
		t.Skip("PostgreSQL 15 extended-schema regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(`
CREATE TABLE options (
  key varchar(191) COLLATE "C",
  value text,
  schema_marker text NOT NULL DEFAULT 'default-marker'
)`).Error)
	require.NoError(t, db.Exec(
		"CREATE INDEX idx_options_schema_marker ON options (schema_marker)",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (key, value, schema_marker) VALUES (?, ?, ?)",
		"extended.option", "preserved-value", "source-specific",
	).Error)

	require.NoError(t, migrateOptionPrimaryKey(db))

	var row struct {
		Key          string
		Value        string
		SchemaMarker string `gorm:"column:schema_marker"`
	}
	require.NoError(t, db.Table("options").First(&row).Error)
	assert.Equal(t, "extended.option", row.Key)
	assert.Equal(t, "preserved-value", row.Value)
	assert.Equal(t, "source-specific", row.SchemaMarker)
	var keyMetadata struct {
		IsNullable    string `gorm:"column:is_nullable"`
		Collation     string `gorm:"column:collation_name"`
		ColumnDefault string `gorm:"column:column_default"`
	}
	require.NoError(t, db.Raw(`
SELECT is_nullable, collation_name, column_default
FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
		"options", "key",
	).Scan(&keyMetadata).Error)
	assert.Equal(t, "NO", keyMetadata.IsNullable)
	assert.Equal(t, "C", keyMetadata.Collation)
	var markerDefault string
	require.NoError(t, db.Raw(`
SELECT column_default
FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
		"options", "schema_marker",
	).Scan(&markerDefault).Error)
	assert.Contains(t, markerDefault, "default-marker")
	var markerIndexes int64
	require.NoError(t, db.Raw(`
SELECT count(*)
FROM pg_catalog.pg_indexes
WHERE schemaname = current_schema() AND tablename = ? AND indexdef LIKE ?`,
		"options", "%(schema_marker)%",
	).Scan(&markerIndexes).Error)
	assert.EqualValues(t, 1, markerIndexes)

	recorder := &migrationSQLRecorder{}
	require.NoError(t, migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder})))
	assert.Empty(t, recorder.schemaMutations())
}

func TestOptionPrimaryKeyMigrationFailsClosedOnPostgresUnloggedRelation(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "postgres" {
		t.Skip("PostgreSQL 15 relation-persistence regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE UNLOGGED TABLE options (key varchar(191), value text)",
	).Error)
	const sourceKey = "unlogged.option"
	const sourceValue = "private-unlogged-value"
	require.NoError(t, db.Exec(
		"INSERT INTO options (key, value) VALUES (?, ?)",
		sourceKey, sourceValue,
	).Error)
	var source struct {
		OID         uint32 `gorm:"column:oid"`
		Persistence string `gorm:"column:persistence"`
	}
	require.NoError(t, db.Raw(`
SELECT oid, relpersistence AS persistence
FROM pg_catalog.pg_class
WHERE oid = to_regclass('options')`).Scan(&source).Error)
	require.Equal(t, "u", source.Persistence)
	recorder := &migrationSQLRecorder{}

	err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

	assert.EqualError(t, err, "PostgreSQL options schema cannot be preserved safely")
	if err != nil {
		assert.NotContains(t, err.Error(), sourceKey)
		assert.NotContains(t, err.Error(), sourceValue)
		assert.NotContains(t, err.Error(), source.Persistence)
	}
	var retained struct {
		OID         uint32 `gorm:"column:oid"`
		Persistence string `gorm:"column:persistence"`
	}
	require.NoError(t, db.Raw(`
SELECT oid, relpersistence AS persistence
FROM pg_catalog.pg_class
WHERE oid = to_regclass('options')`).Scan(&retained).Error)
	assert.Equal(t, source.OID, retained.OID)
	assert.Equal(t, "u", retained.Persistence)
	assert.Empty(t, recorder.schemaMutations(), "relation metadata must fail before temporary DDL")
	assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
	var preservedValue string
	require.NoError(t, db.Table("options").Select("value").
		Where("key = ?", sourceKey).Scan(&preservedValue).Error)
	assert.Equal(t, sourceValue, preservedValue)
}

func TestOptionPrimaryKeyMigrationFailsClosedOnPostgresRelationMetadata(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "postgres" {
		t.Skip("PostgreSQL 15 relation-metadata regression")
	}
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *gorm.DB)
	}{
		{
			name: "non-ordinary relation kind",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(`
CREATE TABLE options (key varchar(191), value text)
PARTITION BY LIST (key)`).Error)
			},
		},
		{
			name: "storage parameter",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(`
CREATE TABLE options (key varchar(191), value text)
WITH (fillfactor = 70)`).Error)
			},
		},
		{
			name: "replica identity",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(
					"CREATE TABLE options (key varchar(191), value text)",
				).Error)
				require.NoError(t, db.Exec(
					"ALTER TABLE options REPLICA IDENTITY FULL",
				).Error)
			},
		},
		{
			name: "inherits parent",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(
					"CREATE TABLE option_defaults (key varchar(191), value text)",
				).Error)
				require.NoError(t, db.Exec(
					"CREATE TABLE options () INHERITS (option_defaults)",
				).Error)
			},
		},
		{
			name: "has child",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(
					"CREATE TABLE options (key varchar(191), value text)",
				).Error)
				require.NoError(t, db.Exec(
					"CREATE TABLE option_overrides () INHERITS (options)",
				).Error)
			},
		},
		{
			name: "typed table",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(
					"CREATE TYPE option_row AS (key varchar(191), value text)",
				).Error)
				require.NoError(t, db.Exec(
					"CREATE TABLE options OF option_row",
				).Error)
			},
		},
		{
			name: "non-default tablespace",
			setup: func(t *testing.T, _ *gorm.DB) {
				t.Skip("isolated PostgreSQL runner provides no external tablespace directory")
			},
		},
		{
			name: "security label",
			setup: func(t *testing.T, _ *gorm.DB) {
				t.Skip("vanilla PostgreSQL 15 image has no security-label provider")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			test.setup(t, db)
			const sourceKey = "relation-metadata.option"
			const sourceValue = "private-relation-metadata-value"
			if test.name != "non-ordinary relation kind" {
				require.NoError(t, db.Exec(
					"INSERT INTO options (key, value) VALUES (?, ?)",
					sourceKey, sourceValue,
				).Error)
			}
			var sourceOID uint32
			require.NoError(t, db.Raw(
				"SELECT to_regclass('options')::oid",
			).Scan(&sourceOID).Error)
			recorder := &migrationSQLRecorder{}

			err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

			assert.EqualError(t, err, "PostgreSQL options schema cannot be preserved safely")
			if err != nil {
				assert.NotContains(t, err.Error(), sourceKey)
				assert.NotContains(t, err.Error(), sourceValue)
			}
			assert.Empty(t, recorder.schemaMutations(), "relation metadata must fail before temporary DDL")
			assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
			var retainedOID uint32
			require.NoError(t, db.Raw(
				"SELECT to_regclass('options')::oid",
			).Scan(&retainedOID).Error)
			assert.Equal(t, sourceOID, retainedOID)
			if test.name != "non-ordinary relation kind" {
				var preservedValue string
				require.NoError(t, db.Table("options").Select("value").
					Where("key = ?", sourceKey).Scan(&preservedValue).Error)
				assert.Equal(t, sourceValue, preservedValue)
			}
		})
	}
}

func TestPostgresOptionMetadataQueryVersionGatesCatalogs(t *testing.T) {
	postgres96 := postgresOptionMetadataQuery("options", 90624)
	for _, requiredPredicate := range []string{
		"relation.relkind",
		"relation.relpersistence",
		"relation.reltablespace",
		"relation.reloptions",
		"relation.relreplident",
		"relation.reloftype",
		"pg_catalog.pg_inherits",
		"pg_catalog.pg_seclabel",
	} {
		assert.Contains(t, postgres96, requiredPredicate)
	}
	assert.Contains(t, postgres96, "relation.relhasoids")
	assert.NotContains(t, postgres96, "pg_publication_rel")
	assert.NotContains(t, postgres96, "default_table_access_method")

	postgres10 := postgresOptionMetadataQuery("options", 100000)
	assert.Contains(t, postgres10, "pg_publication_rel")
	assert.Contains(t, postgres10, "relation.relhasoids")
	assert.NotContains(t, postgres10, "default_table_access_method")

	postgres15 := postgresOptionMetadataQuery("options", 150019)
	assert.Contains(t, postgres15, "pg_publication_rel")
	assert.NotContains(t, postgres15, "relation.relhasoids")
	assert.Contains(t, postgres15, "default_table_access_method")
}

func TestPostgresOptionColumnMetadataQueryVersionGatesCatalogs(t *testing.T) {
	const ownedSequenceProjection = `pg_catalog.pg_get_serial_sequence( ` +
		`pg_catalog.quote_ident(table_schema) || '.' || ` +
		`pg_catalog.quote_ident(table_name), column_name ` +
		`) IS NOT NULL AS has_owned_sequence`
	for _, test := range []struct {
		serverVersion int
		projection    string
	}{
		{serverVersion: 90624, projection: "column_name, 'NO' AS is_identity, 'NEVER' AS is_generated"},
		{serverVersion: 100000, projection: "column_name, is_identity, 'NEVER' AS is_generated"},
		{serverVersion: 110000, projection: "column_name, is_identity, 'NEVER' AS is_generated"},
		{serverVersion: 120000, projection: "column_name, is_identity, is_generated"},
		{serverVersion: 150000, projection: "column_name, is_identity, is_generated"},
	} {
		t.Run(fmt.Sprint(test.serverVersion), func(t *testing.T) {
			query := strings.Join(strings.Fields(
				postgresOptionColumnMetadataQuery(test.serverVersion),
			), " ")
			assert.Equal(t, "SELECT "+test.projection+", "+ownedSequenceProjection+
				" FROM information_schema.columns"+
				" WHERE table_schema = current_schema() AND table_name = ?"+
				" ORDER BY ordinal_position", query)
		})
	}
}

func TestPostgres96OptionMetadataChecksRouteVersionSafeQueries(t *testing.T) {
	catalog := &postgres96CatalogDriver{}
	driverName := fmt.Sprintf("postgres96-catalog-%d", postgres96CatalogDriverSequence.Add(1))
	sql.Register(driverName, catalog)
	sqlDB, err := sql.Open(driverName, "")
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, sqlDB.Close())
	})
	db, err := gorm.Open(
		postgres.New(postgres.Config{Conn: sqlDB, PreferSimpleProtocol: true}),
		&gorm.Config{DisableAutomaticPing: true},
	)
	require.NoError(t, err)

	columns, err := inspectPostgresOptionMigrationSchema(db, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"key", "value"}, columns)
	require.NoError(t, validatePostgresOptionReplacementMetadata(db))

	queries := catalog.recordedQueries()
	versionQueries := 0
	var sourceMetadata, replacementMetadata string
	for _, query := range queries {
		if query == "SHOW server_version_num" {
			versionQueries++
		}
		if !strings.Contains(query, "AS has_unsupported_relation_metadata") {
			continue
		}
		switch {
		case strings.Contains(query, "to_regclass('options_pk_tmp')"):
			replacementMetadata = query
		case strings.Contains(query, "to_regclass('options')"):
			sourceMetadata = query
		}
	}
	assert.Equal(t, 2, versionQueries)
	for name, query := range map[string]string{
		"source":      sourceMetadata,
		"replacement": replacementMetadata,
	} {
		t.Run(name, func(t *testing.T) {
			require.NotEmpty(t, query)
			for _, requiredPredicate := range []string{
				"relation.relkind",
				"relation.relpersistence",
				"relation.reltablespace",
				"relation.reloptions",
				"relation.relreplident",
				"relation.reloftype",
				"relation.relhasoids",
				"pg_catalog.pg_inherits",
				"pg_catalog.pg_seclabel",
				"pg_catalog.pg_description",
				"relacl IS NOT NULL",
				"attacl IS NOT NULL",
				"relowner <>",
			} {
				assert.Contains(t, query, requiredPredicate)
			}
			for _, unsupported := range []string{
				"pg_catalog.pg_publication_rel",
				"relation.relam",
				"pg_catalog.pg_am",
				"default_table_access_method",
			} {
				assert.NotContains(t, query, unsupported)
			}
		})
	}
}

func TestOptionPrimaryKeyMigrationFailsClosedOnPostgresAmbiguousExtendedSchema(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "postgres" {
		t.Skip("PostgreSQL 15 extended-schema duplicate regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(`
CREATE TABLE options (
  key varchar(191),
  value text,
  schema_marker text NOT NULL DEFAULT 'default-marker'
)`).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (key, value, schema_marker) VALUES (?, ?, ?), (?, ?, ?)",
		"duplicate.option", "same-value", "first-marker",
		"duplicate.option", "same-value", "second-marker",
	).Error)

	err := migrateOptionPrimaryKey(db)
	require.ErrorContains(t, err, "extended schema with duplicate keys cannot be migrated safely")
	assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
	var rows []struct {
		SchemaMarker string `gorm:"column:schema_marker"`
	}
	require.NoError(t, db.Table("options").Order("schema_marker").Find(&rows).Error)
	assert.Equal(t, []struct {
		SchemaMarker string `gorm:"column:schema_marker"`
	}{{SchemaMarker: "first-marker"}, {SchemaMarker: "second-marker"}}, rows)
}

func TestOptionPrimaryKeyMigrationFailsClosedOnPostgresGeneratedOrIdentityColumns(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "postgres" {
		t.Skip("PostgreSQL 15 generated/identity regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(`
CREATE TABLE options (
  key varchar(191),
  value text,
  row_id bigint GENERATED ALWAYS AS IDENTITY,
  key_length integer GENERATED ALWAYS AS (char_length(key)) STORED
)`).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (key, value) VALUES (?, ?)",
		"generated.option", "preserved",
	).Error)

	err := migrateOptionPrimaryKey(db)
	require.ErrorContains(t, err, "generated or identity columns cannot be migrated safely")
	assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
	var rowCount int64
	require.NoError(t, db.Table("options").Count(&rowCount).Error)
	assert.EqualValues(t, 1, rowCount)
}

func TestOptionPrimaryKeyMigrationFailsClosedOnPostgresOwnedSequenceDefault(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "postgres" {
		t.Skip("PostgreSQL 15 owned-sequence regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(`
CREATE TABLE options (
  key varchar(191) UNIQUE,
  value text,
  row_id serial
)`).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (key, value) VALUES (?, ?)",
		"sequence.option", "private-option-value",
	).Error)
	var sourceOID uint32
	require.NoError(t, db.Raw(
		"SELECT to_regclass('options')::oid",
	).Scan(&sourceOID).Error)
	var sequenceOwnedBySource bool
	require.NoError(t, db.Raw(`
SELECT EXISTS (
  SELECT 1
  FROM pg_catalog.pg_class AS sequence
  JOIN pg_catalog.pg_depend AS dependency
    ON dependency.classid = 'pg_class'::regclass
   AND dependency.objid = sequence.oid
  JOIN pg_catalog.pg_attribute AS attribute
    ON attribute.attrelid = dependency.refobjid
   AND attribute.attnum = dependency.refobjsubid
  WHERE sequence.relkind = 'S'
    AND dependency.refclassid = 'pg_class'::regclass
    AND dependency.refobjid = to_regclass('options')
    AND dependency.deptype = 'a'
    AND attribute.attname = 'row_id'
)`).Scan(&sequenceOwnedBySource).Error)
	require.True(t, sequenceOwnedBySource, "fixture must own its serial sequence")
	recorder := &migrationSQLRecorder{}

	err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

	require.EqualError(t, err, "PostgreSQL options schema cannot be preserved safely")
	assert.Empty(t, recorder.schemaMutations(), "owned sequence must fail before temporary DDL")
	assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
	var retainedOID uint32
	require.NoError(t, db.Raw(
		"SELECT to_regclass('options')::oid",
	).Scan(&retainedOID).Error)
	assert.Equal(t, sourceOID, retainedOID)
	var sourceValue string
	require.NoError(t, db.Table("options").Select("value").
		Where("key = ?", "sequence.option").Scan(&sourceValue).Error)
	assert.Equal(t, "private-option-value", sourceValue)
}

func TestOptionPrimaryKeyMigrationFailsClosedOnPostgresUncopiedSchemaObjects(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "postgres" {
		t.Skip("PostgreSQL 15 LIKE INCLUDING ALL schema-loss regression")
	}
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *gorm.DB)
	}{
		{
			name: "foreign key",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec("CREATE TABLE option_values (value text PRIMARY KEY)").Error)
				require.NoError(t, db.Exec("INSERT INTO option_values (value) VALUES ('allowed')").Error)
				require.NoError(t, db.Exec(`
CREATE TABLE options (
  key varchar(191),
  value text REFERENCES option_values(value)
)`).Error)
			},
		},
		{
			name: "custom trigger",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec("CREATE TABLE options (key varchar(191), value text)").Error)
				require.NoError(t, db.Exec(`
CREATE FUNCTION options_custom_trigger_fn() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`).Error)
				require.NoError(t, db.Exec(`
CREATE TRIGGER options_custom_bi BEFORE INSERT ON options
FOR EACH ROW EXECUTE FUNCTION options_custom_trigger_fn()`).Error)
			},
		},
		{
			name: "row level security",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec("CREATE TABLE options (key varchar(191), value text)").Error)
				require.NoError(t, db.Exec("ALTER TABLE options ENABLE ROW LEVEL SECURITY").Error)
				require.NoError(t, db.Exec(
					"CREATE POLICY options_visible ON options USING (true)",
				).Error)
			},
		},
		{
			name: "rule",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec("CREATE TABLE options (key varchar(191), value text)").Error)
				require.NoError(t, db.Exec(
					"CREATE RULE options_delete_audit AS ON DELETE TO options DO ALSO NOTHING",
				).Error)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			test.setup(t, db)
			require.NoError(t, db.Exec(
				"INSERT INTO options (key, value) VALUES (?, ?)",
				"schema.option", "allowed",
			).Error)

			err := migrateOptionPrimaryKey(db)
			require.ErrorContains(t, err, "PostgreSQL options schema cannot be preserved safely")
			assert.NotContains(t, err.Error(), "allowed")
			assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
			var value string
			require.NoError(t, db.Table("options").Select("value").
				Where("key = ?", "schema.option").Scan(&value).Error)
			assert.Equal(t, "allowed", value)
			var sourceExists bool
			require.NoError(t, db.Raw(
				"SELECT to_regclass('options') IS NOT NULL",
			).Scan(&sourceExists).Error)
			assert.True(t, sourceExists)
		})
	}
}

func TestOptionPrimaryKeyMigrationFailsClosedOnPostgresTableComment(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "postgres" {
		t.Skip("PostgreSQL 15 table-comment regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (key varchar(191) UNIQUE, value text)",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (key, value) VALUES (?, ?)",
		"comment.option", "private-option-value",
	).Error)
	const tableComment = "private relation annotation"
	require.NoError(t, db.Exec(
		"COMMENT ON TABLE options IS 'private relation annotation'",
	).Error)
	recorder := &migrationSQLRecorder{}

	err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

	assert.EqualError(t, err, "PostgreSQL options schema cannot be preserved safely")
	if err != nil {
		assert.NotContains(t, err.Error(), tableComment)
		assert.NotContains(t, err.Error(), "private-option-value")
	}
	assert.Empty(t, recorder.schemaMutations(), "table comment must fail before temporary DDL")
	assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
	var preservedComment sql.NullString
	require.NoError(t, db.Raw(
		"SELECT obj_description(to_regclass('options'), 'pg_class')",
	).Scan(&preservedComment).Error)
	assert.Equal(t, sql.NullString{String: tableComment, Valid: true}, preservedComment)
	var sourceValue string
	require.NoError(t, db.Table("options").Select("value").
		Where("key = ?", "comment.option").Scan(&sourceValue).Error)
	assert.Equal(t, "private-option-value", sourceValue)
	primary, primaryErr := optionsKeyIsPrimary(db)
	require.NoError(t, primaryErr)
	assert.False(t, primary, "failed migration must retain UNIQUE(key) without promoting it")
}

func TestOptionPrimaryKeyMigrationFailsClosedOnPostgresOIDBoundMetadata(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "postgres" {
		t.Skip("PostgreSQL 15 OID-bound metadata regression")
	}
	t.Run("column ACL", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(
			"CREATE TABLE options (key varchar(191), value text)",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (key, value) VALUES (?, ?)",
			"column-acl.option", "private-option-value",
		).Error)
		require.NoError(t, db.Exec("GRANT SELECT (value) ON options TO PUBLIC").Error)
		var aclMetadata struct {
			HasTableACL  bool `gorm:"column:has_table_acl"`
			HasColumnACL bool `gorm:"column:has_column_acl"`
		}
		require.NoError(t, db.Raw(`
SELECT
  relation.relacl IS NOT NULL AS has_table_acl,
  EXISTS (
    SELECT 1
    FROM pg_catalog.pg_attribute AS attribute
    WHERE attribute.attrelid = relation.oid
      AND attribute.attnum > 0
      AND NOT attribute.attisdropped
      AND attribute.attacl IS NOT NULL
  ) AS has_column_acl
FROM pg_catalog.pg_class AS relation
WHERE relation.oid = to_regclass('options')`).Scan(&aclMetadata).Error)
		require.False(t, aclMetadata.HasTableACL, "fixture must rely only on a column ACL")
		require.True(t, aclMetadata.HasColumnACL)
		recorder := &migrationSQLRecorder{}

		err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

		require.EqualError(t, err, "PostgreSQL options schema cannot be preserved safely")
		assert.NotContains(t, err.Error(), "PUBLIC")
		assert.NotContains(t, err.Error(), "value")
		assert.NotContains(t, err.Error(), "private-option-value")
		assert.Empty(t, recorder.schemaMutations(), "column ACL must fail before temporary DDL")
		assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
		var sourceValue string
		require.NoError(t, db.Table("options").Select("value").
			Where("key = ?", "column-acl.option").Scan(&sourceValue).Error)
		assert.Equal(t, "private-option-value", sourceValue)
	})

	t.Run("table ACL", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(
			"CREATE TABLE options (key varchar(191), value text)",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (key, value) VALUES (?, ?)",
			"acl.option", "private-option-value",
		).Error)
		require.NoError(t, db.Exec("GRANT SELECT ON options TO PUBLIC").Error)
		recorder := &migrationSQLRecorder{}

		err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

		require.ErrorContains(t, err, "PostgreSQL options schema cannot be preserved safely")
		assert.NotContains(t, err.Error(), "private-option-value")
		assert.Empty(t, recorder.schemaMutations(), "ACL must fail before temporary DDL")
		var hasACL bool
		require.NoError(t, db.Raw(`
SELECT relacl IS NOT NULL
FROM pg_catalog.pg_class
WHERE oid = to_regclass('options')`).Scan(&hasACL).Error)
		assert.True(t, hasACL)
	})

	t.Run("non-current owner", func(t *testing.T) {
		db := useMigrationTestDB(t)
		role := fmt.Sprintf("options_owner_%d", time.Now().UnixNano())
		require.NoError(t, db.Exec(`CREATE ROLE "`+role+`"`).Error)
		t.Cleanup(func() {
			_ = db.Exec(`REASSIGN OWNED BY "` + role + `" TO CURRENT_USER`).Error
			_ = db.Exec(`DROP OWNED BY "` + role + `"`).Error
			assert.NoError(t, db.Exec(`DROP ROLE "`+role+`"`).Error)
		})
		require.NoError(t, db.Exec(
			"CREATE TABLE options (key varchar(191), value text)",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (key, value) VALUES (?, ?)",
			"owner.option", "private-option-value",
		).Error)
		require.NoError(t, db.Exec(`ALTER TABLE options OWNER TO "`+role+`"`).Error)
		recorder := &migrationSQLRecorder{}

		err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

		require.ErrorContains(t, err, "PostgreSQL options schema cannot be preserved safely")
		assert.NotContains(t, err.Error(), "private-option-value")
		assert.NotContains(t, err.Error(), role)
		assert.Empty(t, recorder.schemaMutations(), "owner mismatch must fail before temporary DDL")
		var owner string
		require.NoError(t, db.Raw(`
SELECT pg_catalog.pg_get_userbyid(relowner)
FROM pg_catalog.pg_class
WHERE oid = to_regclass('options')`).Scan(&owner).Error)
		assert.Equal(t, role, owner)
	})

	t.Run("explicit publication membership", func(t *testing.T) {
		db := useMigrationTestDB(t)
		publication := fmt.Sprintf("options_publication_%d", time.Now().UnixNano())
		t.Cleanup(func() {
			assert.NoError(t, db.Exec(`DROP PUBLICATION IF EXISTS "`+publication+`"`).Error)
		})
		require.NoError(t, db.Exec(
			"CREATE TABLE options (key varchar(191), value text)",
		).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (key, value) VALUES (?, ?)",
			"publication.option", "private-option-value",
		).Error)
		require.NoError(t, db.Exec(
			`CREATE PUBLICATION "`+publication+`" FOR TABLE options`,
		).Error)
		recorder := &migrationSQLRecorder{}

		err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

		require.ErrorContains(t, err, "PostgreSQL options schema cannot be preserved safely")
		assert.NotContains(t, err.Error(), "private-option-value")
		assert.NotContains(t, err.Error(), publication)
		assert.Empty(t, recorder.schemaMutations(), "publication membership must fail before temporary DDL")
		var memberships int64
		require.NoError(t, db.Raw(`
SELECT count(*)
FROM pg_catalog.pg_publication_rel
WHERE prpubid = (SELECT oid FROM pg_catalog.pg_publication WHERE pubname = ?)
  AND prrelid = to_regclass('options')`, publication).Scan(&memberships).Error)
		assert.EqualValues(t, 1, memberships)
	})
}

func TestOptionPrimaryKeyMigrationFailsClosedOnPostgresDefaultPrivileges(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "postgres" {
		t.Skip("PostgreSQL 15 default-privilege regression")
	}
	db := useMigrationTestDB(t)
	var schemaName string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&schemaName).Error)
	require.True(t, optionSafeIdent(schemaName))
	var defaultACLsBefore int64
	require.NoError(t, db.Raw(`
SELECT count(*)
FROM pg_catalog.pg_default_acl
WHERE defaclrole = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user)
  AND defaclnamespace = to_regnamespace(?)
  AND defaclobjtype = 'r'`, schemaName).Scan(&defaultACLsBefore).Error)

	rollbackFixture := errors.New("rollback default-privilege fixture")
	transactionErr := db.Transaction(func(tx *gorm.DB) error {
		require.NoError(t, tx.Exec(
			"CREATE TABLE options (key varchar(191) UNIQUE, value text)",
		).Error)
		const sourceKey = "default-acl.option"
		const sourceValue = "private-default-acl-value"
		require.NoError(t, tx.Exec(
			"INSERT INTO options (key, value) VALUES (?, ?)",
			sourceKey, sourceValue,
		).Error)
		var sourceOID uint32
		require.NoError(t, tx.Raw("SELECT to_regclass('options')::oid").Scan(&sourceOID).Error)
		var sourceACL struct {
			HasTableACL  bool `gorm:"column:has_table_acl"`
			HasColumnACL bool `gorm:"column:has_column_acl"`
		}
		require.NoError(t, tx.Raw(`
SELECT
  relation.relacl IS NOT NULL AS has_table_acl,
  EXISTS (
    SELECT 1
    FROM pg_catalog.pg_attribute AS attribute
    WHERE attribute.attrelid = relation.oid
      AND attribute.attnum > 0
      AND NOT attribute.attisdropped
      AND attribute.attacl IS NOT NULL
  ) AS has_column_acl
FROM pg_catalog.pg_class AS relation
WHERE relation.oid = to_regclass('options')`).Scan(&sourceACL).Error)
		require.False(t, sourceACL.HasTableACL)
		require.False(t, sourceACL.HasColumnACL)

		require.NoError(t, tx.Exec(
			`ALTER DEFAULT PRIVILEGES IN SCHEMA "`+schemaName+`" GRANT SELECT ON TABLES TO PUBLIC`,
		).Error)
		require.NoError(t, tx.Exec("CREATE TABLE options_default_acl_probe (id bigint)").Error)
		var probeHasACL bool
		require.NoError(t, tx.Raw(`
SELECT relacl IS NOT NULL
FROM pg_catalog.pg_class
WHERE oid = to_regclass('options_default_acl_probe')`).Scan(&probeHasACL).Error)
		require.True(t, probeHasACL, "new tables must receive the configured default ACL")
		require.NoError(t, tx.Exec("DROP TABLE options_default_acl_probe").Error)

		migrationErr := migrateOptionPrimaryKey(tx)

		assert.EqualError(t, migrationErr, "PostgreSQL options schema cannot be preserved safely")
		if migrationErr != nil {
			assert.NotContains(t, migrationErr.Error(), "PUBLIC")
			assert.NotContains(t, migrationErr.Error(), sourceKey)
			assert.NotContains(t, migrationErr.Error(), sourceValue)
		}
		assert.False(t, tx.Migrator().HasTable(optionPrimaryKeyTmpTable))
		var preservedOID uint32
		require.NoError(t, tx.Raw("SELECT to_regclass('options')::oid").Scan(&preservedOID).Error)
		assert.Equal(t, sourceOID, preservedOID)
		var columnCount int64
		require.NoError(t, tx.Raw(`
SELECT count(*)
FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = 'options'`).Scan(&columnCount).Error)
		assert.EqualValues(t, 2, columnCount)
		var preservedValue string
		require.NoError(t, tx.Table("options").Select("value").
			Where("key = ?", sourceKey).Scan(&preservedValue).Error)
		assert.Equal(t, sourceValue, preservedValue)
		var preservedACL struct {
			HasTableACL  bool `gorm:"column:has_table_acl"`
			HasColumnACL bool `gorm:"column:has_column_acl"`
		}
		require.NoError(t, tx.Raw(`
SELECT
  relation.relacl IS NOT NULL AS has_table_acl,
  EXISTS (
    SELECT 1
    FROM pg_catalog.pg_attribute AS attribute
    WHERE attribute.attrelid = relation.oid
      AND attribute.attnum > 0
      AND NOT attribute.attisdropped
      AND attribute.attacl IS NOT NULL
  ) AS has_column_acl
FROM pg_catalog.pg_class AS relation
WHERE relation.oid = to_regclass('options')`).Scan(&preservedACL).Error)
		assert.False(t, preservedACL.HasTableACL)
		assert.False(t, preservedACL.HasColumnACL)
		primary, primaryErr := optionsKeyIsPrimary(tx)
		require.NoError(t, primaryErr)
		assert.False(t, primary, "failed migration must retain UNIQUE(key) without promoting it")
		return rollbackFixture
	})
	require.ErrorIs(t, transactionErr, rollbackFixture)

	var defaultACLsAfter int64
	require.NoError(t, db.Raw(`
SELECT count(*)
FROM pg_catalog.pg_default_acl
WHERE defaclrole = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user)
  AND defaclnamespace = to_regnamespace(?)
  AND defaclobjtype = 'r'`, schemaName).Scan(&defaultACLsAfter).Error)
	assert.Equal(t, defaultACLsBefore, defaultACLsAfter)
}

func TestOptionPrimaryKeyMigrationFailsClosedOnPostgresDependentViews(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "postgres" {
		t.Skip("PostgreSQL 15 dependent-view regression")
	}
	for _, test := range []struct {
		name    string
		kind    string
		relkind string
	}{
		{name: "view", kind: "VIEW", relkind: "v"},
		{name: "materialized view", kind: "MATERIALIZED VIEW", relkind: "m"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options (key varchar(191) UNIQUE, value text)",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (key, value) VALUES ('view.option', 'preserved')",
			).Error)
			require.NoError(t, db.Exec(
				"CREATE "+test.kind+" options_dependent_view AS SELECT key, value FROM options",
			).Error)
			recorder := &migrationSQLRecorder{}

			err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

			require.ErrorContains(t, err, "PostgreSQL options schema cannot be preserved safely")
			assert.Empty(t, recorder.schemaMutations(), "dependent views must fail before temporary DDL")
			assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
			var sourceValue string
			require.NoError(t, db.Table("options").Select("value").
				Where("key = ?", "view.option").Scan(&sourceValue).Error)
			assert.Equal(t, "preserved", sourceValue)
			var dependentCount int64
			require.NoError(t, db.Raw(`
SELECT count(*)
FROM pg_catalog.pg_rewrite AS rewrite_meta
JOIN pg_catalog.pg_class AS dependent
  ON dependent.oid = rewrite_meta.ev_class
JOIN pg_catalog.pg_depend AS dependency
  ON dependency.classid = 'pg_rewrite'::regclass
 AND dependency.objid = rewrite_meta.oid
WHERE dependent.relname = 'options_dependent_view'
  AND dependent.relkind = ?
  AND dependency.refclassid = 'pg_class'::regclass
  AND dependency.refobjid = to_regclass('options')`, test.relkind).Scan(&dependentCount).Error)
			assert.Positive(t, dependentCount, "view must still depend on the original options relation")
		})
	}
}

func TestOptionPrimaryKeyMigrationFailsClosedOnUnsupportedSQLiteSchema(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite unsupported-schema regression")
	}
	for _, test := range []struct {
		name   string
		create string
		index  string
	}{
		{
			name: "extra column",
			create: "CREATE TABLE options (" +
				"`key` varchar(191), `value` text, `schema_marker` text NOT NULL DEFAULT 'default-marker')",
		},
		{
			name: "generated extra column",
			create: "CREATE TABLE options (" +
				"`key` varchar(191), `value` text, " +
				"`key_length` integer GENERATED ALWAYS AS (length(`key`)) STORED)",
		},
		{
			name:   "unexpected index",
			create: "CREATE TABLE options (`key` varchar(191), `value` text)",
			index:  "CREATE INDEX idx_options_value ON options (`value`)",
		},
		{
			name:   "custom index collation",
			create: "CREATE TABLE options (`key` varchar(191), `value` text)",
			index:  "CREATE UNIQUE INDEX idx_options_key_nocase ON options (`key` COLLATE NOCASE)",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(test.create).Error)
			if test.index != "" {
				require.NoError(t, db.Exec(test.index).Error)
			}
			if test.name == "extra column" {
				require.NoError(t, db.Exec(
					"INSERT INTO options (`key`, `value`, `schema_marker`) VALUES (?, ?, ?)",
					"sqlite.option", "preserved", "source-specific",
				).Error)
			} else {
				require.NoError(t, db.Exec(
					"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
					"sqlite.option", "preserved",
				).Error)
			}

			err := migrateOptionPrimaryKey(db)
			require.ErrorContains(t, err, "SQLite options schema cannot be preserved safely")
			assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
			var rowCount int64
			require.NoError(t, db.Table("options").Count(&rowCount).Error)
			assert.EqualValues(t, 1, rowCount)
			if test.name == "extra column" {
				var marker string
				require.NoError(t, db.Table("options").Select("schema_marker").Scan(&marker).Error)
				assert.Equal(t, "source-specific", marker)
			}
		})
	}
}

func TestOptionPrimaryKeyMigrationRejectsSQLiteConflictClauses(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite CREATE TABLE conflict-clause regression")
	}
	t.Run("column not null on conflict ignore", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(`
CrEaTe TaBlE "options" (
  "key" VARCHAR(191) NOT NULL,
  [value] TEXT NoT NULL /* quoted identifier; ON CONFLICT ABORT is comment text */
    oN CoNfLiCt IgNoRe,
  PRIMARY KEY ("key", [value])
)`).Error)
		require.NoError(t, db.Exec(
			`INSERT INTO "options" ("key", [value]) VALUES (?, ?)`,
			"preserved.option", "preserved",
		).Error)
		assertIgnoredNull := func(t *testing.T) {
			t.Helper()
			result := db.Exec(
				`INSERT INTO "options" ("key", [value]) VALUES (?, NULL)`,
				"ignored.option",
			)
			require.NoError(t, result.Error)
			assert.Zero(t, result.RowsAffected)
			var ignored int64
			require.NoError(t, db.Table("options").
				Where("key = ?", "ignored.option").Count(&ignored).Error)
			assert.Zero(t, ignored)
		}
		assertIgnoredNull(t)
		var sourceSQL string
		require.NoError(t, db.Raw(
			"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'options'",
		).Scan(&sourceSQL).Error)
		recorder := &migrationSQLRecorder{}

		err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

		require.ErrorContains(t, err, "SQLite options schema cannot be preserved safely")
		assert.Empty(t, recorder.schemaMutations(), "conflict clause must fail before temporary DDL")
		assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
		var artifacts int64
		require.NoError(t, db.Raw(`
SELECT count(*) FROM sqlite_master
WHERE type = 'table' AND (name = ? OR name LIKE ?)`,
			optionPrimaryKeyTmpTable, optionLegacyTablePrefix+"%",
		).Scan(&artifacts).Error)
		assert.Zero(t, artifacts)
		var preservedSQL string
		require.NoError(t, db.Raw(
			"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'options'",
		).Scan(&preservedSQL).Error)
		assert.Equal(t, sourceSQL, preservedSQL)
		assertIgnoredNull(t)
	})

	t.Run("composite primary key on conflict replace", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(`
CREATE TABLE [options] (
  `+"`key`"+` VARCHAR(191) NOT NULL,
  "value" TEXT NOT NULL,
  CONSTRAINT "options composite key"
    PRIMARY KEY ([key], `+"`value`"+`) /* ON CONFLICT IGNORE is comment text */
    On CoNfLiCt RePlAcE
)`).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (`key`, `value`) VALUES (?, ?), (?, ?)",
			"replace.option", "preserved",
			"sentinel.option", "sentinel",
		).Error)
		assertReplaced := func(t *testing.T) {
			t.Helper()
			var before int64
			require.NoError(t, db.Raw(
				"SELECT rowid FROM options WHERE `key` = ? AND `value` = ?",
				"replace.option", "preserved",
			).Scan(&before).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
				"replace.option", "preserved",
			).Error)
			var after int64
			require.NoError(t, db.Raw(
				"SELECT rowid FROM options WHERE `key` = ? AND `value` = ?",
				"replace.option", "preserved",
			).Scan(&after).Error)
			assert.Greater(t, after, before)
			var rows int64
			require.NoError(t, db.Table("options").Count(&rows).Error)
			assert.EqualValues(t, 2, rows)
		}
		assertReplaced(t)
		var sourceSQL string
		require.NoError(t, db.Raw(
			"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'options'",
		).Scan(&sourceSQL).Error)
		recorder := &migrationSQLRecorder{}

		err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

		require.ErrorContains(t, err, "SQLite options schema cannot be preserved safely")
		assert.Empty(t, recorder.schemaMutations(), "conflict clause must fail before temporary DDL")
		assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
		var artifacts int64
		require.NoError(t, db.Raw(`
SELECT count(*) FROM sqlite_master
WHERE type = 'table' AND (name = ? OR name LIKE ?)`,
			optionPrimaryKeyTmpTable, optionLegacyTablePrefix+"%",
		).Scan(&artifacts).Error)
		assert.Zero(t, artifacts)
		var preservedSQL string
		require.NoError(t, db.Raw(
			"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'options'",
		).Scan(&preservedSQL).Error)
		assert.Equal(t, sourceSQL, preservedSQL)
		assertReplaced(t)
	})
}

func TestOptionPrimaryKeyMigrationAllowsUnrelatedSQLiteDMLConflictClauses(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite DML conflict-clause scope regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(`
CREATE TABLE "options" (
  "key" VARCHAR(191) UNIQUE,
  [value] TEXT /* ON CONFLICT FAIL is comment text */
)`).Error)
	require.NoError(t, db.Exec(
		`INSERT INTO "options" ("key", [value]) VALUES (?, ?)`,
		"source.option", "preserved",
	).Error)
	require.NoError(t, db.Exec(
		"CREATE TABLE conflict_events (id INTEGER PRIMARY KEY, payload TEXT NOT NULL)",
	).Error)
	require.NoError(t, db.Exec(
		"CREATE TABLE conflict_audit (`key` TEXT PRIMARY KEY, value TEXT NOT NULL)",
	).Error)
	require.NoError(t, db.Exec(`
CREATE TRIGGER unrelated_conflict_trigger AFTER INSERT ON conflict_events
BEGIN
  INSERT INTO conflict_audit (`+"`key`"+`, value) VALUES ('event', NEW.payload)
  ON CONFLICT (`+"`key`"+`) DO UPDATE SET value = excluded.value;
END`).Error)

	require.NoError(t, migrateOptionPrimaryKey(db))

	require.NoError(t, db.Exec(
		"INSERT INTO conflict_events (id, payload) VALUES (1, 'first'), (2, 'second')",
	).Error)
	var auditValue string
	require.NoError(t, db.Table("conflict_audit").Select("value").
		Where("`key` = ?", "event").Scan(&auditValue).Error)
	assert.Equal(t, "second", auditValue)
	var sourceValue string
	require.NoError(t, db.Table("options").Select("value").
		Where("key = ?", "source.option").Scan(&sourceValue).Error)
	assert.Equal(t, "preserved", sourceValue)
}

func TestOptionPrimaryKeyMigrationFailsClosedOnSQLiteUncopiedSchemaObjects(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite table reconstruction schema-loss regression")
	}
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *gorm.DB)
	}{
		{
			name: "value constraints and default",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(
					"CREATE TABLE options ("+
						"`key` varchar(191),"+
						"`value` text NOT NULL DEFAULT 'default-value' CHECK (length(`value`) > 0)"+
						")",
				).Error)
			},
		},
		{
			name: "key not null",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(
					"CREATE TABLE options (`key` varchar(191) NOT NULL, `value` text)",
				).Error)
			},
		},
		{
			name: "foreign key",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec("CREATE TABLE option_values (`value` text PRIMARY KEY)").Error)
				require.NoError(t, db.Exec("INSERT INTO option_values (`value`) VALUES ('allowed')").Error)
				require.NoError(t, db.Exec(
					"CREATE TABLE options ("+
						"`key` varchar(191), `value` text REFERENCES option_values(`value`)"+
						")",
				).Error)
			},
		},
		{
			name: "custom trigger",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(
					"CREATE TABLE options (`key` varchar(191), `value` text)",
				).Error)
				require.NoError(t, db.Exec(`
CREATE TRIGGER options_custom_ai AFTER INSERT ON options
BEGIN
  UPDATE options SET value = NEW.value WHERE rowid = NEW.rowid;
END`).Error)
			},
		},
		{
			name: "strict table",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(
					"CREATE TABLE options (`key` TEXT, `value` TEXT) STRICT",
				).Error)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			test.setup(t, db)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
				"schema.option", "allowed",
			).Error)

			err := migrateOptionPrimaryKey(db)
			require.ErrorContains(t, err, "SQLite options schema cannot be preserved safely")
			assert.NotContains(t, err.Error(), "allowed")
			assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
			var value string
			require.NoError(t, db.Table("options").Select("`value`").
				Where("`key` = ?", "schema.option").Scan(&value).Error)
			assert.Equal(t, "allowed", value)
			var sourceSQL string
			require.NoError(t, db.Raw(
				"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'options'",
			).Scan(&sourceSQL).Error)
			assert.NotEmpty(t, sourceSQL)
		})
	}
}

func TestOptionPrimaryKeyMigrationFailsClosedOnSQLiteIncomingDependencies(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite incoming dependency regression")
	}
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *gorm.DB)
		check func(*testing.T, *gorm.DB)
	}{
		{
			name: "incoming foreign key",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(`
CREATE TABLE option_children (
  id integer PRIMARY KEY,
  option_key varchar(191),
  FOREIGN KEY (option_key) REFERENCES options(key)
)`).Error)
				require.NoError(t, db.Exec(
					"INSERT INTO option_children (id, option_key) VALUES (1, 'child.option')",
				).Error)
			},
			check: func(t *testing.T, db *gorm.DB) {
				var referencedTable string
				require.NoError(t, db.Raw(`
SELECT "table" FROM pragma_foreign_key_list('option_children') LIMIT 1`,
				).Scan(&referencedTable).Error)
				assert.Equal(t, "options", referencedTable)
				var childKey string
				require.NoError(t, db.Table("option_children").Select("option_key").
					Where("id = 1").Scan(&childKey).Error)
				assert.Equal(t, "child.option", childKey)
			},
		},
		{
			name: "dependent view",
			setup: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(
					"CREATE VIEW options_dependent_view AS SELECT key, value FROM options",
				).Error)
			},
			check: func(t *testing.T, db *gorm.DB) {
				var viewSQL string
				require.NoError(t, db.Raw(`
SELECT sql FROM sqlite_master WHERE type = 'view' AND name = 'options_dependent_view'`,
				).Scan(&viewSQL).Error)
				assert.Contains(t, strings.ToLower(viewSQL), "from options")
				var value string
				require.NoError(t, db.Table("options_dependent_view").Select("value").
					Where("key = ?", "child.option").Scan(&value).Error)
				assert.Equal(t, "preserved", value)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options (`key` varchar(191) UNIQUE, `value` text)",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES ('child.option', 'preserved')",
			).Error)
			test.setup(t, db)
			recorder := &migrationSQLRecorder{}

			err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

			require.ErrorContains(t, err, "SQLite options schema cannot be preserved safely")
			assert.Empty(t, recorder.schemaMutations(), "incoming dependencies must fail before temporary DDL")
			assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
			var value string
			require.NoError(t, db.Table("options").Select("value").
				Where("key = ?", "child.option").Scan(&value).Error)
			assert.Equal(t, "preserved", value)
			test.check(t, db)
		})
	}
}

func TestOptionPrimaryKeyMigrationFailsClosedOnSQLiteIncomingTriggerRelations(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite incoming trigger dependency regression")
	}
	for _, test := range []struct {
		name       string
		body       string
		verifyFire func(*testing.T, *gorm.DB)
	}{
		{
			name: "insert unquoted",
			body: "INSERT INTO options (`key`, `value`) VALUES ('inserted.option', 'inserted')",
			verifyFire: func(t *testing.T, db *gorm.DB) {
				var value string
				require.NoError(t, db.Table("options").Select("value").
					Where("key = ?", "inserted.option").Scan(&value).Error)
				assert.Equal(t, "inserted", value)
			},
		},
		{
			name: `update double quoted`,
			body: "INSERT INTO trigger_audit (value) VALUES ('before-update');\n" +
				`UPDATE "options" SET value = 'updated' WHERE key = 'target.option'`,
			verifyFire: func(t *testing.T, db *gorm.DB) {
				var value string
				require.NoError(t, db.Table("options").Select("value").
					Where("key = ?", "target.option").Scan(&value).Error)
				assert.Equal(t, "updated", value)
				var audit string
				require.NoError(t, db.Table("trigger_audit").Select("value").Scan(&audit).Error)
				assert.Equal(t, "before-update", audit)
			},
		},
		{
			name: "update or rollback unquoted",
			body: "UPDATE OR ROLLBACK options SET value = 'rollback' WHERE key = 'target.option'",
			verifyFire: func(t *testing.T, db *gorm.DB) {
				var value string
				require.NoError(t, db.Table("options").Select("value").
					Where("key = ?", "target.option").Scan(&value).Error)
				assert.Equal(t, "rollback", value)
			},
		},
		{
			name: "update or abort double quoted",
			body: `UPDATE OR ABORT "options" SET value = 'abort' WHERE key = 'target.option'`,
			verifyFire: func(t *testing.T, db *gorm.DB) {
				var value string
				require.NoError(t, db.Table("options").Select("value").
					Where("key = ?", "target.option").Scan(&value).Error)
				assert.Equal(t, "abort", value)
			},
		},
		{
			name: "update or replace backtick quoted",
			body: "UPDATE OR REPLACE `options` SET value = 'replace' WHERE key = 'target.option'",
			verifyFire: func(t *testing.T, db *gorm.DB) {
				var value string
				require.NoError(t, db.Table("options").Select("value").
					Where("key = ?", "target.option").Scan(&value).Error)
				assert.Equal(t, "replace", value)
			},
		},
		{
			name: "update or fail bracket quoted",
			body: "UPDATE OR FAIL [options] SET value = 'fail' WHERE key = 'target.option'",
			verifyFire: func(t *testing.T, db *gorm.DB) {
				var value string
				require.NoError(t, db.Table("options").Select("value").
					Where("key = ?", "target.option").Scan(&value).Error)
				assert.Equal(t, "fail", value)
			},
		},
		{
			name: "update or ignore unquoted",
			body: "UPDATE OR IGNORE options SET value = 'ignore' WHERE key = 'target.option'",
			verifyFire: func(t *testing.T, db *gorm.DB) {
				var value string
				require.NoError(t, db.Table("options").Select("value").
					Where("key = ?", "target.option").Scan(&value).Error)
				assert.Equal(t, "ignore", value)
			},
		},
		{
			name: "delete backtick quoted",
			body: "DELETE FROM `options` WHERE key = 'target.option'",
			verifyFire: func(t *testing.T, db *gorm.DB) {
				var rows int64
				require.NoError(t, db.Table("options").
					Where("key = ?", "target.option").Count(&rows).Error)
				assert.Zero(t, rows)
			},
		},
		{
			name: "select from bracket quoted",
			body: "INSERT INTO trigger_audit (value) " +
				"SELECT value FROM [options] WHERE key = 'target.option'",
			verifyFire: func(t *testing.T, db *gorm.DB) {
				var value string
				require.NoError(t, db.Table("trigger_audit").Select("value").Scan(&value).Error)
				assert.Equal(t, "preserved", value)
			},
		},
		{
			name: "select join",
			body: "INSERT INTO trigger_audit (value) " +
				"SELECT source.value FROM trigger_events AS event " +
				"JOIN options AS source ON source.key = 'target.option' " +
				"WHERE event.id = NEW.id",
			verifyFire: func(t *testing.T, db *gorm.DB) {
				var value string
				require.NoError(t, db.Table("trigger_audit").Select("value").Scan(&value).Error)
				assert.Equal(t, "preserved", value)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options (`key` varchar(191) UNIQUE, `value` text)",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES ('target.option', 'preserved')",
			).Error)
			require.NoError(t, db.Exec(
				"CREATE TABLE trigger_events (id integer PRIMARY KEY)",
			).Error)
			require.NoError(t, db.Exec(
				"CREATE TABLE trigger_audit (value text)",
			).Error)
			require.NoError(t, db.Exec(
				"CREATE TRIGGER incoming_options_trigger AFTER INSERT ON trigger_events BEGIN\n"+
					test.body+";\nEND",
			).Error)
			var triggerSQL string
			require.NoError(t, db.Raw(
				"SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?",
				"incoming_options_trigger",
			).Scan(&triggerSQL).Error)
			recorder := &migrationSQLRecorder{}

			err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

			require.ErrorContains(t, err, "SQLite options schema cannot be preserved safely")
			assert.Empty(t, recorder.schemaMutations(), "incoming trigger must fail before temporary DDL")
			var preservedTriggerSQL string
			require.NoError(t, db.Raw(
				"SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?",
				"incoming_options_trigger",
			).Scan(&preservedTriggerSQL).Error)
			assert.Equal(t, triggerSQL, preservedTriggerSQL)
			require.NoError(t, db.Exec("INSERT INTO trigger_events (id) VALUES (1)").Error)
			test.verifyFire(t, db)
		})
	}
}

func TestSQLiteQueryRelationParserScopesUpdateConflictClause(t *testing.T) {
	for _, test := range []struct {
		name           string
		statement      string
		wantReference  bool
		wantCompletely bool
	}{
		{
			name:           "ordinary update",
			statement:      "UPDATE options SET value = 'updated'",
			wantReference:  true,
			wantCompletely: true,
		},
		{
			name:           "conflict update",
			statement:      `UPDATE OR IGNORE "options" SET value = 'updated'`,
			wantReference:  true,
			wantCompletely: true,
		},
		{
			name:           "OR column outside update relation",
			statement:      `SELECT "OR", options FROM unrelated`,
			wantReference:  false,
			wantCompletely: true,
		},
		{
			name:           "missing conflict action",
			statement:      "UPDATE OR options SET value = 'updated'",
			wantReference:  false,
			wantCompletely: false,
		},
		{
			name:           "missing update relation",
			statement:      "UPDATE OR FAIL",
			wantReference:  false,
			wantCompletely: false,
		},
		{
			name:           "duplicate conflict separator",
			statement:      "UPDATE OR IGNORE OR options SET value = 'updated'",
			wantReference:  false,
			wantCompletely: false,
		},
		{
			name:           "quoted action is not grammar",
			statement:      `UPDATE OR "IGNORE" options SET value = 'updated'`,
			wantReference:  false,
			wantCompletely: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tokens, complete := sqliteLexSQL(test.statement)
			require.True(t, complete)

			references, parsed := sqliteQueryReferencesTable(tokens, "OPTIONS", nil)

			assert.Equal(t, test.wantReference, references)
			assert.Equal(t, test.wantCompletely, parsed)
		})
	}
}

func TestOptionPrimaryKeyMigrationAllowsUnrelatedSQLiteTriggerOptionsText(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite unrelated trigger regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (`key` varchar(191) UNIQUE, `value` text)",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
	).Error)
	require.NoError(t, db.Exec("CREATE TABLE trigger_events (id integer PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("CREATE TABLE trigger_audit (value text)").Error)
	require.NoError(t, db.Exec(`
CREATE TRIGGER unrelated_options_trigger AFTER INSERT ON trigger_events
BEGIN
  INSERT INTO trigger_audit (value) VALUES ('options');
  -- INSERT INTO options and SELECT FROM options are only comment text.
END`).Error)
	var triggerSQL string
	require.NoError(t, db.Raw(
		"SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?",
		"unrelated_options_trigger",
	).Scan(&triggerSQL).Error)

	require.NoError(t, migrateOptionPrimaryKey(db))

	var preservedTriggerSQL string
	require.NoError(t, db.Raw(
		"SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?",
		"unrelated_options_trigger",
	).Scan(&preservedTriggerSQL).Error)
	assert.Equal(t, triggerSQL, preservedTriggerSQL)
	require.NoError(t, db.Exec("INSERT INTO trigger_events (id) VALUES (1)").Error)
	var value string
	require.NoError(t, db.Table("trigger_audit").Select("value").Scan(&value).Error)
	assert.Equal(t, "options", value)
}

func TestOptionPrimaryKeyMigrationFailsClosedOnSQLiteQuotedDependentViews(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite quoted dependent-view regression")
	}
	for _, test := range []struct {
		name       string
		identifier string
	}{
		{name: "double quoted", identifier: `"options"`},
		{name: "backtick quoted", identifier: "`options`"},
		{name: "bracket quoted", identifier: `[options]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options (`key` varchar(191) UNIQUE, `value` text)",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES ('view.option', 'preserved')",
			).Error)
			require.NoError(t, db.Exec(
				"CREATE VIEW options_quoted_view AS SELECT key, value FROM "+test.identifier,
			).Error)
			recorder := &migrationSQLRecorder{}

			err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

			require.ErrorContains(t, err, "SQLite options schema cannot be preserved safely")
			assert.Empty(t, recorder.schemaMutations(), "quoted dependent view must fail before temporary DDL")
			var value string
			require.NoError(t, db.Table("options_quoted_view").Select("value").
				Where("key = ?", "view.option").Scan(&value).Error)
			assert.Equal(t, "preserved", value)
		})
	}
}

func TestOptionPrimaryKeyMigrationFailsClosedOnSQLiteViewRelationContexts(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite view relation-context regression")
	}
	for _, test := range []struct {
		name  string
		query string
	}{
		{name: "unquoted from", query: "SELECT key FROM options"},
		{name: "from with alias", query: "SELECT source.key FROM options AS source"},
		{name: "join", query: "SELECT unrelated.id FROM unrelated JOIN options ON 1 = 1"},
		{name: "schema qualified", query: "SELECT key FROM main.options"},
		{name: "nested subquery", query: "SELECT key FROM (SELECT key FROM options)"},
		{name: "comma from list", query: "SELECT unrelated.id FROM unrelated, options"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options (`key` varchar(191) UNIQUE, `value` text)",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES ('view.option', 'preserved')",
			).Error)
			require.NoError(t, db.Exec(
				"CREATE TABLE unrelated (id integer PRIMARY KEY)",
			).Error)
			require.NoError(t, db.Exec("INSERT INTO unrelated (id) VALUES (1)").Error)
			require.NoError(t, db.Exec(
				"CREATE VIEW options_relation_view AS "+test.query,
			).Error)
			recorder := &migrationSQLRecorder{}

			err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

			require.ErrorContains(t, err, "SQLite options schema cannot be preserved safely")
			assert.Empty(t, recorder.schemaMutations(), "dependent view must fail before temporary DDL")
			assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
			var value string
			require.NoError(t, db.Table("options").Select("value").
				Where("key = ?", "view.option").Scan(&value).Error)
			assert.Equal(t, "preserved", value)
		})
	}
}

func TestOptionPrimaryKeyMigrationAllowsSQLiteCTEShadowing(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite CTE shadowing regression")
	}
	for _, test := range []struct {
		name  string
		query string
	}{
		{
			name:  "simple",
			query: "WITH options AS (SELECT id FROM unrelated) SELECT id FROM options",
		},
		{
			name: "recursive",
			query: "WITH RECURSIVE options(id) AS (" +
				"SELECT id FROM unrelated UNION ALL " +
				"SELECT id + 1 FROM options WHERE id < 2" +
				") SELECT id FROM options",
		},
		{
			name: "multiple",
			query: "WITH first AS (SELECT id FROM unrelated), " +
				"options AS (SELECT id FROM first) SELECT id FROM options",
		},
		{
			name:  "quoted",
			query: `WITH "options" AS (SELECT id FROM unrelated) SELECT id FROM "options"`,
		},
		{
			name: "forward reference",
			query: "WITH first AS (SELECT id FROM options), " +
				"options AS (SELECT id FROM unrelated) SELECT id FROM first",
		},
		{
			name: "quoted forward reference",
			query: `WITH first AS (SELECT id FROM "options"), ` +
				`"options" AS (SELECT id FROM unrelated) SELECT id FROM first`,
		},
		{
			name: "multiple forward reference",
			query: "WITH first AS (SELECT id FROM options), " +
				"second AS (SELECT id FROM first), " +
				"options AS (SELECT id FROM unrelated) SELECT id FROM second",
		},
		{
			name: "recursive forward reference",
			query: "WITH RECURSIVE first AS (SELECT id FROM options), " +
				"options(id) AS (SELECT id FROM unrelated) SELECT id FROM first",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options (`key` varchar(191) UNIQUE, `value` text)",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
			).Error)
			require.NoError(t, db.Exec("CREATE TABLE unrelated (id integer PRIMARY KEY)").Error)
			require.NoError(t, db.Exec("INSERT INTO unrelated (id) VALUES (1)").Error)
			require.NoError(t, db.Exec(
				"CREATE VIEW cte_shadow_view AS "+test.query,
			).Error)

			require.NoError(t, migrateOptionPrimaryKey(db))

			var ids []int
			require.NoError(t, db.Table("cte_shadow_view").Select("id").Order("id").Scan(&ids).Error)
			require.NotEmpty(t, ids)
			assert.Equal(t, 1, ids[0])
		})
	}
}

func TestOptionPrimaryKeyMigrationRejectsSQLitePhysicalOptionsWithCTEs(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite CTE physical-dependency regression")
	}
	for _, test := range []struct {
		name  string
		query string
	}{
		{
			name:  "physical dependency in CTE body",
			query: "WITH visible AS (SELECT key FROM options) SELECT key FROM visible",
		},
		{
			name: "schema qualified physical dependency shadows CTE",
			query: "WITH options AS (SELECT id AS key FROM unrelated) " +
				"SELECT key FROM main.options",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options (`key` varchar(191) UNIQUE, `value` text)",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES ('view.option', 'preserved')",
			).Error)
			require.NoError(t, db.Exec("CREATE TABLE unrelated (id integer PRIMARY KEY)").Error)
			require.NoError(t, db.Exec("INSERT INTO unrelated (id) VALUES (1)").Error)
			require.NoError(t, db.Exec(
				"CREATE VIEW cte_physical_view AS "+test.query,
			).Error)
			recorder := &migrationSQLRecorder{}

			err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

			require.ErrorContains(t, err, "SQLite options schema cannot be preserved safely")
			assert.Empty(t, recorder.schemaMutations(), "physical CTE dependency must fail before temporary DDL")
		})
	}
}

func TestSQLiteViewRelationParserFailsClosedOnIncompleteCTE(t *testing.T) {
	for _, statement := range []string{
		"CREATE VIEW incomplete AS WITH options AS SELECT id FROM unrelated",
		"CREATE VIEW incomplete AS WITH options AS () SELECT id FROM options",
		"CREATE VIEW incomplete AS WITH options AS (SELECT id FROM unrelated)",
		"CREATE VIEW incomplete AS WITH options(id AS (SELECT id FROM unrelated) SELECT id FROM options",
	} {
		_, parsed := sqliteViewReferencesTable(statement, "options")
		assert.False(t, parsed, statement)
	}
}

func TestOptionPrimaryKeyMigrationAllowsSQLiteViewColumnsNamedOptions(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite view column lexer regression")
	}
	for _, test := range []struct {
		name       string
		identifier string
	}{
		{name: "unquoted", identifier: "options"},
		{name: "double quoted", identifier: `"options"`},
		{name: "backtick quoted", identifier: "`options`"},
		{name: "bracket quoted", identifier: `[options]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options (`key` varchar(191) UNIQUE, `value` text)",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
			).Error)
			require.NoError(t, db.Exec("CREATE TABLE unrelated (options text)").Error)
			require.NoError(t, db.Exec(
				"INSERT INTO unrelated (options) VALUES ('column-preserved')",
			).Error)
			require.NoError(t, db.Exec(
				"CREATE VIEW unrelated_view AS SELECT options."+test.identifier+
					" AS options FROM unrelated AS options "+
					"WHERE 'FROM options' <> '' /* JOIN options */",
			).Error)

			require.NoError(t, migrateOptionPrimaryKey(db))

			var value string
			require.NoError(t, db.Table("unrelated_view").Select("options").Scan(&value).Error)
			assert.Equal(t, "column-preserved", value)
		})
	}
}

func TestOptionPrimaryKeyMigrationAllowsSQLiteViewStringLiteral(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite string-literal lexer regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (`key` varchar(191) UNIQUE, `value` text)",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
	).Error)
	require.NoError(t, db.Exec(
		"CREATE VIEW unrelated_options_literal AS SELECT 'options' AS label",
	).Error)

	require.NoError(t, migrateOptionPrimaryKey(db))

	var label string
	require.NoError(t, db.Table("unrelated_options_literal").Select("label").Scan(&label).Error)
	assert.Equal(t, "options", label)
}

func TestSQLiteViewRelationParserFailsClosedOnIncompleteSQL(t *testing.T) {
	for _, statement := range []string{
		"CREATE VIEW incomplete_comment AS SELECT options FROM unrelated /*",
		"CREATE VIEW incomplete_string AS SELECT 'options",
		`CREATE VIEW incomplete_identifier AS SELECT "options`,
		"CREATE VIEW incomplete_relation AS SELECT options FROM",
	} {
		_, parsed := sqliteViewReferencesTable(statement, "options")
		assert.False(t, parsed, statement)
	}
}

func TestOptionPrimaryKeyMigrationAllowsUnrelatedSQLiteForeignKeys(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite unrelated foreign-key regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (`key` varchar(191) UNIQUE, `value` text)",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES ('source.option', 'preserved')",
	).Error)
	require.NoError(t, db.Exec("CREATE TABLE unrelated_parents (id integer PRIMARY KEY)").Error)
	require.NoError(t, db.Exec(`
CREATE TABLE unrelated_children (
  id integer PRIMARY KEY,
  parent_id integer REFERENCES unrelated_parents(id)
)`).Error)

	require.NoError(t, migrateOptionPrimaryKey(db))

	primary, err := optionsKeyIsPrimary(db)
	require.NoError(t, err)
	assert.True(t, primary)
	var referencedTable string
	require.NoError(t, db.Raw(`
SELECT "table" FROM pragma_foreign_key_list('unrelated_children') LIMIT 1`,
	).Scan(&referencedTable).Error)
	assert.Equal(t, "unrelated_parents", referencedTable)
}

func TestOptionPrimaryKeyMigrationFailsClosedOnConflictingDuplicates(t *testing.T) {
	db := useMigrationTestDB(t)
	createLegacyOptions(t, db, []legacyOptionWithoutPrimaryKey{
		{Key: "RetryTimes", Value: "first-value"},
		{Key: "RetryTimes", Value: "second-value"},
	})

	err := migrateDB()
	require.Error(t, err)
	assert.Regexp(t, `^options primary key conflict: key="RetryTimes" rows=2 value_digest=hmac-sha256:[0-9a-f]{64}$`, err.Error())
	t.Logf("conflict diagnostic: %s", err)
	assert.Contains(t, err.Error(), `key="RetryTimes"`)
	assert.Contains(t, err.Error(), "rows=2")
	assert.Contains(t, err.Error(), "value_digest=hmac-sha256:")
	assert.NotContains(t, err.Error(), "first-value")
	assert.NotContains(t, err.Error(), "second-value")
	assert.False(t, db.Migrator().HasTable(&Channel{}), "main AutoMigrate must not run after an options conflict")

	var rows int64
	require.NoError(t, db.Table("options").Count(&rows).Error)
	assert.EqualValues(t, 2, rows, "fail-closed startup must preserve the conflicting rows")
	assertNoMySQLOptionMigrationArtifacts(t, db)
}

func TestOptionPrimaryKeyMigrationConflictDigestResistsOfflineEnumeration(t *testing.T) {
	db := useMigrationTestDB(t)
	values := []string{"false", "true"}
	createLegacyOptions(t, db, []legacyOptionWithoutPrimaryKey{
		{Key: "low-entropy", Value: values[0]},
		{Key: "low-entropy", Value: values[1]},
	})
	err := migrateOptionPrimaryKey(db)
	require.Error(t, err)
	match := regexp.MustCompile(`value_digest=[^:]+:([0-9a-f]{64})$`).FindStringSubmatch(err.Error())
	require.Len(t, match, 2)

	slices.Sort(values)
	hash := sha256.New()
	var length [8]byte
	for _, value := range values {
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	offlineDigest := hex.EncodeToString(hash.Sum(nil))

	assert.NotEqual(t, offlineDigest, match[1], "digest must not permit offline verification of candidate values")
	assert.NotContains(t, err.Error(), values[0])
	assert.NotContains(t, err.Error(), values[1])
}

func TestOptionPrimaryKeyMigrationFailsClosedWhenEntropyFails(t *testing.T) {
	db := useMigrationTestDB(t)
	createLegacyOptions(t, db, []legacyOptionWithoutPrimaryKey{
		{Key: "RetryTimes", Value: "same"},
		{Key: "RetryTimes", Value: "same"},
	})

	err := repairOptionPrimaryKey(db, failingOptionEntropyReader{})
	require.EqualError(t, err, "generate options conflict digest key: entropy unavailable")
	assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
	assert.False(t, db.Migrator().HasTable(&Channel{}), "main AutoMigrate must not run after entropy failure")
}

func TestOptionPrimaryKeyMigrationConvergesSameValueDuplicates(t *testing.T) {
	db := useMigrationTestDB(t)
	sourceRows := []legacyOptionWithoutPrimaryKey{
		{Key: "RetryTimes", Value: "same-value"},
		{Key: "RetryTimes", Value: "same-value"},
	}
	createLegacyOptions(t, db, sourceRows)

	require.NoError(t, migrateOptionPrimaryKey(db))
	var options []Option
	require.NoError(t, db.Order(clause.OrderByColumn{Column: clause.Column{Name: "key"}}).Find(&options).Error)
	assert.Equal(t, []Option{{Key: "RetryTimes", Value: "same-value"}}, options)
	primary, err := optionsKeyIsPrimary(db)
	require.NoError(t, err)
	assert.True(t, primary)
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" {
		retained := mysqlOptionTerminalRetainedTable(t, db)
		var retainedRows []legacyOptionWithoutPrimaryKey
		require.NoError(t, db.Table(retained).Find(&retainedRows).Error)
		assert.Equal(t, sourceRows, retainedRows)
		assert.False(t, mysqlOptionTerminalJournalExists(t, db))

		beforeRestart := captureMySQLOptionRecoverySnapshot(t, db, retained)
		recorder := &migrationSQLRecorder{}
		require.NoError(t, migrateOptionPrimaryKey(db.Session(&gorm.Session{
			NewDB: true, Logger: recorder,
		})))
		assert.Empty(t, recorder.schemaMutations())
		assert.Equal(t, beforeRestart, captureMySQLOptionRecoverySnapshot(t, db, retained))
	}
}

func TestOptionPrimaryKeyMigrationUsesMySQLCollationEquivalence(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 utf8mb4_unicode_ci regression")
	}
	for _, test := range []struct {
		name      string
		canonical string
		alias     string
	}{
		{name: "case protected", canonical: "RetryTimes", alias: "retrytimes"},
		{name: "trailing space protected", canonical: "RetryTimes", alias: "RetryTimes "},
		{name: "accent protected", canonical: "AutomaticRetryStatusCodes", alias: "\u00c1utomaticRetryStatusCodes"},
		{name: "binary ordinary", canonical: "Foo", alias: "foo"},
	} {
		t.Run(test.name+" same value", func(t *testing.T) {
			db := useMigrationTestDB(t)
			sourceRows := []legacyOptionWithoutPrimaryKey{
				{Key: test.alias, Value: "same"},
				{Key: test.canonical, Value: "same"},
			}
			createLegacyOptions(t, db, sourceRows)

			require.NoError(t, migrateOptionPrimaryKey(db))
			var options []Option
			require.NoError(t, db.Find(&options).Error)
			assert.Equal(t, []Option{{Key: test.canonical, Value: "same"}}, options)
			retained := mysqlOptionTerminalRetainedTable(t, db)
			var retainedRows []legacyOptionWithoutPrimaryKey
			require.NoError(t, db.Table(retained).Find(&retainedRows).Error)
			assert.ElementsMatch(t, sourceRows, retainedRows)
			assert.False(t, mysqlOptionTerminalJournalExists(t, db))

			beforeRestart := captureMySQLOptionRecoverySnapshot(t, db, retained)
			recorder := &migrationSQLRecorder{}
			require.NoError(t, migrateOptionPrimaryKey(db.Session(&gorm.Session{
				NewDB: true, Logger: recorder,
			})))
			assert.Empty(t, recorder.schemaMutations())
			assert.Equal(t, beforeRestart, captureMySQLOptionRecoverySnapshot(t, db, retained))
		})

		t.Run(test.name+" different values", func(t *testing.T) {
			db := useMigrationTestDB(t)
			createLegacyOptions(t, db, []legacyOptionWithoutPrimaryKey{
				{Key: test.alias, Value: "one"},
				{Key: test.canonical, Value: "two"},
			})

			err := migrateDB()
			require.Error(t, err)
			assert.Regexp(t,
				`^options primary key conflict: key="`+regexp.QuoteMeta(test.canonical)+`" rows=2 value_digest=hmac-sha256:[0-9a-f]{64}$`,
				err.Error(),
			)
			assert.NotContains(t, err.Error(), "one")
			assert.NotContains(t, err.Error(), "two")
			assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
			assert.False(t, db.Migrator().HasTable(&Channel{}), "main AutoMigrate must not run after an options conflict")
		})
	}
}

func TestOptionPrimaryKeyMigrationPreservesDeduplicatedMySQLFullRows(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 full-row deduplication proof regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options ("+
			"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
			"`value` longtext,"+
			"`binary_marker` varbinary(32) NOT NULL,"+
			"`nullable_marker` varbinary(32) NULL"+
			") ENGINE=InnoDB",
	).Error)
	binaryMarker := []byte{0x00, 0xff, 0x10, 0x80}
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`, `binary_marker`, `nullable_marker`)"+
			" VALUES (?, ?, ?, NULL), (?, ?, ?, NULL)",
		"retrytimes", "same", binaryMarker,
		"RetryTimes", "same", binaryMarker,
	).Error)

	require.NoError(t, migrateOptionPrimaryKey(db))

	type fullRow struct {
		Key            string         `gorm:"column:key"`
		Value          string         `gorm:"column:value"`
		BinaryMarker   []byte         `gorm:"column:binary_marker"`
		NullableMarker sql.NullString `gorm:"column:nullable_marker"`
	}
	var activeRows []fullRow
	require.NoError(t, db.Table("options").Find(&activeRows).Error)
	require.Equal(t, []fullRow{{
		Key: "RetryTimes", Value: "same", BinaryMarker: binaryMarker,
	}}, activeRows)

	retained := mysqlOptionTerminalRetainedTable(t, db)
	var retainedRows []fullRow
	require.NoError(t, db.Table(retained).Order("BINARY `key`").Find(&retainedRows).Error)
	require.Equal(t, []fullRow{
		{Key: "RetryTimes", Value: "same", BinaryMarker: binaryMarker},
		{Key: "retrytimes", Value: "same", BinaryMarker: binaryMarker},
	}, retainedRows)
	assert.False(t, mysqlOptionTerminalJournalExists(t, db))

	beforeRestart := captureMySQLOptionRecoverySnapshot(t, db, retained)
	recorder := &migrationSQLRecorder{}
	require.NoError(t, migrateOptionPrimaryKey(db.Session(&gorm.Session{
		NewDB: true, Logger: recorder,
	})))
	assert.Empty(t, recorder.schemaMutations())
	assert.Equal(t, beforeRestart, captureMySQLOptionRecoverySnapshot(t, db, retained))
}

func TestOptionPrimaryKeyMigrationTerminalJournalRejectsDeduplicatedRowDrift(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 deduplicated terminal row proof regression")
	}
	tests := []struct {
		name  string
		drift func(*testing.T, *gorm.DB, string)
	}{
		{
			name: "active raw row",
			drift: func(t *testing.T, db *gorm.DB, _ string) {
				require.NoError(t, db.Exec(
					"UPDATE options SET `binary_marker` = ? WHERE BINARY `key` = BINARY ?",
					[]byte("private-active-row"), "RetryTimes",
				).Error)
			},
		},
		{
			name: "retained noncanonical raw row",
			drift: func(t *testing.T, db *gorm.DB, retained string) {
				require.NoError(t, db.Exec(
					"UPDATE "+quoteMySQLIdent(retained)+
						" SET `binary_marker` = ? WHERE BINARY `key` = BINARY ?",
					[]byte("private-retained-raw-row"), "retrytimes",
				).Error)
			},
		},
		{
			name: "retained canonical row",
			drift: func(t *testing.T, db *gorm.DB, retained string) {
				require.NoError(t, db.Exec(
					"UPDATE "+quoteMySQLIdent(retained)+
						" SET `binary_marker` = ? WHERE BINARY `key` = BINARY ?",
					[]byte("private-retained-canonical-row"), "RetryTimes",
				).Error)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options ("+
					"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,"+
					"`value` longtext,"+
					"`binary_marker` varbinary(32) NOT NULL"+
					") ENGINE=InnoDB",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`, `binary_marker`) VALUES (?, ?, ?), (?, ?, ?)",
				"retrytimes", "same", []byte{0x00, 0xff},
				"RetryTimes", "same", []byte{0x00, 0xff},
			).Error)
			crashMySQLOptionFinalizationAt(
				t, db, "DROP COLUMN `"+optionMigrationStateCol+"`",
			)
			retained := mysqlOptionTerminalRetainedTable(t, db)
			journal, exists, err := inspectMySQLOptionTerminalJournal(db)
			require.NoError(t, err)
			require.True(t, exists)
			require.True(t, journal.hasSnapshot())
			digests, err := mysqlOptionTerminalRowDigests(
				db, mysqlOptionMigrationArtifacts{BackupTable: retained},
			)
			require.NoError(t, err)
			assert.Equal(t, digests.ActiveRaw, journal.ActiveRowsSHA256)
			assert.Equal(t, digests.RetainedRaw, journal.RetainedRowsSHA256)
			assert.Equal(t,
				digests.RetainedCanonicalSemantic,
				journal.RetainedCanonicalRowsSHA256,
			)
			assert.Equal(t, digests.ActiveSemantic, digests.RetainedCanonicalSemantic)
			assert.NotEqual(t, journal.ActiveRowsSHA256, journal.RetainedCanonicalRowsSHA256)
			assert.NotEqual(t, journal.ActiveRowsSHA256, journal.RetainedRowsSHA256)
			test.drift(t, db, retained)
			before := captureMySQLOptionRecoverySnapshot(t, db, retained)
			recorder := &migrationSQLRecorder{}

			err = migrateOptionPrimaryKey(db.Session(&gorm.Session{
				NewDB: true, Logger: recorder,
			}))

			require.Error(t, err)
			assert.NotContains(t, err.Error(), "private-")
			assert.Empty(t, recorder.schemaMutations())
			assert.Equal(t, before, captureMySQLOptionRecoverySnapshot(t, db, retained))
			assert.True(t, mysqlOptionTerminalJournalExists(t, db))
		})
	}
}

func TestOptionPrimaryKeyMigrationFailsClosedOnMySQLNullKeys(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 nullable UNIQUE-key regression")
	}
	for _, test := range []struct {
		name      string
		rowCount  int
		errorExpr string
	}{
		{
			name:      "row limit cannot be bypassed",
			rowCount:  optionMigrationMaxRows + 1,
			errorExpr: `^read options rows: options row limit exceeded: table=options rows=10001 maximum=10000$`,
		},
		{
			name:      "small set is rejected before swap",
			rowCount:  2,
			errorExpr: `^options primary key conflict: key="" rows=2 value_digest=hmac-sha256:[0-9a-f]{64}$`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Table("options").Migrator().CreateTable(&legacyOptionWithNullableKey{}))
			rows := make([]legacyOptionWithNullableKey, test.rowCount)
			for i := range rows {
				rows[i].Value = "same-value"
			}
			require.NoError(t, db.Table("options").CreateInBatches(rows, 100).Error)

			err := migrateDB()
			if assert.Error(t, err) {
				assert.Regexp(t, test.errorExpr, err.Error())
				assert.NotContains(t, err.Error(), "same-value")
			}
			assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
			assert.False(t, db.Migrator().HasTable(&Channel{}), "main AutoMigrate must not run after invalid options")

			var preserved int64
			require.NoError(t, db.Table("options").Count(&preserved).Error)
			assert.EqualValues(t, test.rowCount, preserved, "fail-closed startup must preserve every source row")
			var preservedNulls int64
			require.NoError(t, db.Table("options").
				Where(clause.Eq{Column: clause.Column{Name: "key"}, Value: nil}).
				Count(&preservedNulls).Error)
			assert.EqualValues(t, test.rowCount, preservedNulls, "fail-closed startup must preserve every NULL key")
			assertNoMySQLOptionMigrationArtifacts(t, db)
		})
	}
}

func TestOptionPrimaryKeyMigrationFailsClosedOnMySQLNullValuesBeforeArtifacts(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("MySQL 5.7 nullable value regression")
	}
	for _, test := range []struct {
		name string
		rows []struct {
			key   string
			value any
		}
	}{
		{
			name: "collation aliases distinguish null and empty",
			rows: []struct {
				key   string
				value any
			}{
				{key: "Alias.Option", value: nil},
				{key: "alias.option", value: ""},
			},
		},
		{
			name: "single null is invalid",
			rows: []struct {
				key   string
				value any
			}{
				{key: "nullable.option", value: nil},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options ("+
					"`key` varchar(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci,"+
					"`value` longtext NULL"+
					") ENGINE=InnoDB",
			).Error)
			for _, row := range test.rows {
				require.NoError(t, db.Exec(
					"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
					row.key, row.value,
				).Error)
			}
			recorder := &migrationSQLRecorder{}

			err := migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder}))

			require.Error(t, err)
			assert.Regexp(t,
				`^options primary key conflict: key="[^"]+" rows=[12] value_digest=hmac-sha256:[0-9a-f]{64}$`,
				err.Error(),
			)
			assert.Empty(t, recorder.schemaMutations(), "nullable values must fail before temporary DDL")
			assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
			var preserved []struct {
				Key   string         `gorm:"column:key"`
				Value sql.NullString `gorm:"column:value"`
			}
			require.NoError(t, db.Table("options").Order("BINARY `key`").Find(&preserved).Error)
			require.Len(t, preserved, len(test.rows))
			for i, row := range test.rows {
				assert.Equal(t, row.key, preserved[i].Key)
				if row.value == nil {
					assert.False(t, preserved[i].Value.Valid)
				} else {
					assert.Equal(t, sql.NullString{String: row.value.(string), Valid: true}, preserved[i].Value)
				}
			}
		})
	}
}

func TestOptionValuesDigestDistinguishesNullAndEmpty(t *testing.T) {
	key := []byte(strings.Repeat("k", optionDigestKeySize))
	nullDigest := optionValuesDigest(key, []sql.NullString{{}})
	emptyDigest := optionValuesDigest(key, []sql.NullString{{String: "", Valid: true}})
	assert.NotEqual(t, nullDigest, emptyDigest)
}

func TestOptionPrimaryKeyMigrationPromotesUniqueIndexToPrimaryKey(t *testing.T) {
	db := useMigrationTestDB(t)
	require.NoError(t, db.Table("options").Migrator().CreateTable(&legacyOptionWithUniqueKey{}))
	rows := []legacyOptionWithUniqueKey{
		{Key: "FirstOption", Value: "first-value"},
		{Key: "SecondOption", Value: "second-value"},
	}
	require.NoError(t, db.Table("options").Create(&rows).Error)

	keyColumnIsPrimary := func() bool {
		columns, err := db.Migrator().ColumnTypes(&Option{})
		require.NoError(t, err)
		for _, column := range columns {
			if column.Name() != "key" {
				continue
			}
			primary, ok := column.PrimaryKey()
			return ok && primary
		}
		require.Fail(t, "options key column not found")
		return false
	}
	require.False(t, keyColumnIsPrimary(), "fixture must start with UNIQUE(key), not PRIMARY KEY(key)")

	require.NoError(t, migrateOptionPrimaryKey(db))
	assert.True(t, keyColumnIsPrimary(), "migration completion requires PRIMARY KEY(key)")
	var options []Option
	require.NoError(t, db.Order(clause.OrderByColumn{Column: clause.Column{Name: "key"}}).Find(&options).Error)
	assert.Equal(t, []Option{
		{Key: "FirstOption", Value: "first-value"},
		{Key: "SecondOption", Value: "second-value"},
	}, options)

	recorder := &migrationSQLRecorder{}
	require.NoError(t, migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder})))
	assert.Empty(t, recorder.schemaMutations(), "second migration must not mutate a primary-key-complete table")
}

func TestOptionPrimaryKeyMigrationRebuildsSQLiteCompositePrimaryKey(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite composite-primary-key regression")
	}
	t.Run("different values fail startup closed", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(`
CREATE TABLE options (
  key TEXT NOT NULL,
  value TEXT NOT NULL,
  PRIMARY KEY (key, value)
)`).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (key, value) VALUES (?, ?), (?, ?)",
			"APP_PLUGIN_V1_ENABLED", "false",
			"APP_PLUGIN_V1_ENABLED", "true",
		).Error)

		err := migrateDB()

		require.Error(t, err)
		assert.Regexp(t,
			`^options primary key conflict: key="APP_PLUGIN_V1_ENABLED" rows=2 value_digest=hmac-sha256:[0-9a-f]{64}$`,
			err.Error(),
		)
		assert.NotContains(t, err.Error(), "false")
		assert.NotContains(t, err.Error(), "true")
		assert.False(t, db.Migrator().HasTable(&Channel{}),
			"main AutoMigrate must not run after a composite-primary-key conflict")
		var rows []Option
		require.NoError(t, db.Table("options").Order("value").Find(&rows).Error)
		assert.Equal(t, []Option{
			{Key: "APP_PLUGIN_V1_ENABLED", Value: "false"},
			{Key: "APP_PLUGIN_V1_ENABLED", Value: "true"},
		}, rows)
		assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
	})

	t.Run("nonconflicting rows rebuild once", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, db.Exec(`
CREATE TABLE options (
  key TEXT NOT NULL,
  value TEXT NOT NULL,
  PRIMARY KEY (key, value)
)`).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO options (key, value) VALUES (?, ?), (?, ?)",
			"APP_PLUGIN_V1_ENABLED", "false",
			"RetryTimes", "3",
		).Error)

		require.NoError(t, migrateOptionPrimaryKey(db))

		var columns []sqliteOptionColumn
		require.NoError(t, db.Raw(`
SELECT name, type, "notnull" AS not_null, dflt_value AS default_value,
       pk AS primary_key, hidden
FROM pragma_table_xinfo(?)
ORDER BY cid`, "options").Scan(&columns).Error)
		require.Len(t, columns, 2)
		assert.Equal(t, "key", columns[0].Name)
		assert.Equal(t, 1, columns[0].NotNull)
		assert.Equal(t, 1, columns[0].PrimaryKey)
		assert.Equal(t, "value", columns[1].Name)
		assert.Equal(t, 1, columns[1].NotNull)
		assert.Zero(t, columns[1].PrimaryKey)
		var options []Option
		require.NoError(t, db.Order(clause.OrderByColumn{Column: clause.Column{Name: "key"}}).Find(&options).Error)
		assert.Equal(t, []Option{
			{Key: "APP_PLUGIN_V1_ENABLED", Value: "false"},
			{Key: "RetryTimes", Value: "3"},
		}, options)

		recorder := &migrationSQLRecorder{}
		require.NoError(t, migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder})))
		assert.Empty(t, recorder.schemaMutations(),
			"second migration must not mutate a sole-key-primary table")
	})
}

func TestOptionPrimaryKeyMigrationMakesSQLiteKeyExplicitlyNotNull(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite primary-key nullability regression")
	}
	for _, keyType := range []string{"TEXT", "VARCHAR(191)"} {
		t.Run(keyType, func(t *testing.T) {
			db := useMigrationTestDB(t)
			require.NoError(t, db.Exec(
				"CREATE TABLE options (`key` "+keyType+", `value` TEXT)",
			).Error)
			require.NoError(t, db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES (?, ?)",
				"legacy.option", "preserved",
			).Error)

			require.NoError(t, migrateOptionPrimaryKey(db))

			var columns []sqliteOptionColumn
			require.NoError(t, db.Raw(`
SELECT name, type, "notnull" AS not_null, dflt_value AS default_value,
       pk AS primary_key, hidden
FROM pragma_table_xinfo(?)
ORDER BY cid`, "options").Scan(&columns).Error)
			require.Len(t, columns, 2)
			assert.Equal(t, "key", columns[0].Name)
			assert.Equal(t, keyType, strings.ToUpper(columns[0].Type))
			assert.Equal(t, 1, columns[0].NotNull)
			assert.Equal(t, 1, columns[0].PrimaryKey)
			assert.Equal(t, "value", columns[1].Name)
			assert.Equal(t, "TEXT", strings.ToUpper(columns[1].Type))
			assert.Zero(t, columns[1].NotNull)
			assert.Zero(t, columns[1].PrimaryKey)

			err := db.Exec(
				"INSERT INTO options (`key`, `value`) VALUES (NULL, 'rejected')",
			).Error
			require.Error(t, err)
			var nullKeys int64
			require.NoError(t, db.Table("options").
				Where(clause.Eq{Column: clause.Column{Name: "key"}, Value: nil}).
				Count(&nullKeys).Error)
			assert.Zero(t, nullKeys)
			var value string
			require.NoError(t, db.Table("options").Select("value").
				Where("key = ?", "legacy.option").Scan(&value).Error)
			assert.Equal(t, "preserved", value)

			recorder := &migrationSQLRecorder{}
			require.NoError(t, migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder})))
			assert.Empty(t, recorder.schemaMutations(),
				"second migration must not mutate a primary-key-complete table")
		})
	}
}

func TestSQLiteOptionNullablePrimaryKeyNullRowsFailClosed(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite nullable primary-key regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (`key` TEXT PRIMARY KEY, `value` TEXT)",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (NULL, 'first'), (NULL, 'second')",
	).Error)

	err := migrateDB()
	require.Error(t, err)
	assert.Regexp(t,
		`^options primary key conflict: key="" rows=2 value_digest=hmac-sha256:[0-9a-f]{64}$`,
		err.Error(),
	)
	assert.NotContains(t, err.Error(), "first")
	assert.NotContains(t, err.Error(), "second")
	assert.False(t, db.Migrator().HasTable(&Channel{}),
		"main AutoMigrate must not run after invalid options")

	var rows int64
	require.NoError(t, db.Table("options").Count(&rows).Error)
	assert.EqualValues(t, 2, rows)
	var nullKeys int64
	require.NoError(t, db.Table("options").
		Where(clause.Eq{Column: clause.Column{Name: "key"}, Value: nil}).
		Count(&nullKeys).Error)
	assert.EqualValues(t, 2, nullKeys)
	assert.False(t, db.Migrator().HasTable(optionPrimaryKeyTmpTable))
}

func TestSQLiteOptionNullablePrimaryKeyValidRowsRebuildOnce(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite nullable primary-key regression")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE options (`key` TEXT PRIMARY KEY, `value` TEXT)",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES ('first', 'one'), ('second', 'two')",
	).Error)

	require.NoError(t, migrateOptionPrimaryKey(db))

	var columns []sqliteOptionColumn
	require.NoError(t, db.Raw(`
SELECT name, type, "notnull" AS not_null, dflt_value AS default_value,
       pk AS primary_key, hidden
FROM pragma_table_xinfo(?)
ORDER BY cid`, "options").Scan(&columns).Error)
	require.Len(t, columns, 2)
	assert.Equal(t, "key", columns[0].Name)
	assert.Equal(t, 1, columns[0].NotNull)
	assert.Equal(t, 1, columns[0].PrimaryKey)
	var options []Option
	require.NoError(t, db.Order(clause.OrderByColumn{Column: clause.Column{Name: "key"}}).Find(&options).Error)
	assert.Equal(t, []Option{
		{Key: "first", Value: "one"},
		{Key: "second", Value: "two"},
	}, options)

	recorder := &migrationSQLRecorder{}
	require.NoError(t, migrateOptionPrimaryKey(db.Session(&gorm.Session{Logger: recorder})))
	assert.Empty(t, recorder.schemaMutations(),
		"second migration must not mutate a primary-key-complete table")
}

func TestSQLiteFreshMigrationCreatesNotNullOptionPrimaryKey(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") == "mysql" ||
		os.Getenv("APP_PLUGIN_TEST_DIALECT") == "postgres" {
		t.Skip("SQLite primary-key nullability regression")
	}
	db := useMigrationTestDB(t)

	require.NoError(t, migrateDB())

	var columns []sqliteOptionColumn
	require.NoError(t, db.Raw(`
SELECT name, type, "notnull" AS not_null, dflt_value AS default_value,
       pk AS primary_key, hidden
FROM pragma_table_xinfo(?)
ORDER BY cid`, "options").Scan(&columns).Error)
	require.Len(t, columns, 2)
	assert.Equal(t, "key", columns[0].Name)
	assert.Equal(t, 1, columns[0].NotNull)
	assert.Equal(t, 1, columns[0].PrimaryKey)
	require.Error(t, db.Exec(
		"INSERT INTO options (`key`, `value`) VALUES (NULL, 'rejected')",
	).Error)
}

func TestProtectedOptionWritersRejectCollationAliases(t *testing.T) {
	db := useMigrationTestDB(t)
	require.NoError(t, db.AutoMigrate(&Option{}))
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = map[string]string{}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})

	for _, test := range []struct {
		name             string
		canonical        string
		alias            string
		databaseSpecific bool
	}{
		{name: "passkey case", canonical: "passkey.rp_id", alias: "PASSKEY.RP_ID"},
		{name: "request policy case", canonical: "RetryTimes", alias: "retrytimes"},
		{name: "request policy space", canonical: "RetryTimes", alias: "RetryTimes "},
		{
			name:             "request policy accent",
			canonical:        "AutomaticRetryStatusCodes",
			alias:            "\u00c1utomaticRetryStatusCodes",
			databaseSpecific: true,
		},
		{name: "app policy case", canonical: AppModelInvokePolicyKey, alias: strings.ToLower(AppModelInvokePolicyKey)},
		{name: "app rollout case", canonical: "APP_PLUGIN_V1_ENABLED", alias: "app_plugin_v1_enabled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, db.Where(clause.Eq{Column: "key", Value: test.canonical}).
				Delete(&Option{}).Error)
			require.NoError(t, db.Create(&Option{Key: test.canonical, Value: "unchanged"}).Error)
			if test.databaseSpecific {
				var equivalent int64
				require.NoError(t, db.Model(&Option{}).
					Where(clause.Eq{Column: "key", Value: test.alias}).Count(&equivalent).Error)
				if equivalent == 0 {
					t.Skip("database collation keeps this alias distinct")
				}
			}

			err := UpdateOption(test.alias, "replacement")
			require.ErrorContains(t, err, "protected option")

			var stored Option
			require.NoError(t, db.Where(clause.Eq{Column: "key", Value: test.canonical}).First(&stored).Error)
			assert.Equal(t, "unchanged", stored.Value)
			var options []Option
			require.NoError(t, db.Find(&options).Error)
			assert.NotContains(t, options, Option{Key: test.alias, Value: "replacement"})
		})
	}
}

func TestProtectedOptionCanonicalWritersRejectStoredCollationAliases(t *testing.T) {
	db := useMigrationTestDB(t)
	if db.Dialector.Name() == "sqlite" {
		require.NoError(t, db.Exec(
			`CREATE TABLE options ("key" TEXT COLLATE NOCASE PRIMARY KEY NOT NULL, "value" TEXT)`,
		).Error)
	} else {
		require.NoError(t, db.AutoMigrate(&Option{}))
	}
	require.NoError(t, db.AutoMigrate(&AppExecutionPolicyVersion{}, &PasskeyCredential{}))

	previousSnapshot := CurrentRequestPolicy()
	previousRetryTimes := common.RetryTimes
	previousPasskeySettings := *system_setting.GetPasskeySettings()
	previousServerAddress := system_setting.ServerAddress
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = maps.Clone(previousSnapshot.Options)
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		requestPolicySnapshot.Store(previousSnapshot)
		common.RetryTimes = previousRetryTimes
		*system_setting.GetPasskeySettings() = previousPasskeySettings
		system_setting.ServerAddress = previousServerAddress
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})

	type writerCase struct {
		name       string
		canonical  string
		alias      string
		aliasValue string
		value      string
		write      func() error
	}
	requestCases := []writerCase{
		{name: "Request Policy lowercase", canonical: "RetryTimes", alias: "retrytimes", aliasValue: "3", value: "7"},
		{name: "Request Policy trailing", canonical: "RetryTimes", alias: "RetryTimes ", aliasValue: "3", value: "7"},
		{name: "Request Policy accent", canonical: "RetryTimes", alias: "R\u00e9tryTimes", aliasValue: "3", value: "7"},
	}
	for i := range requestCases {
		test := &requestCases[i]
		test.write = func() error {
			return UpdateRequestPolicyOptions(map[string]string{test.canonical: test.value})
		}
	}
	cases := append(requestCases,
		writerCase{
			name: "Passkey", canonical: "passkey.rp_id", alias: "PASSKEY.RP_ID",
			aliasValue: "example.com", value: "login.example.com",
			write: func() error {
				_, err := UpdatePasskeyDomainOptions(
					map[string]string{"passkey.rp_id": "login.example.com"}, false, "",
				)
				return err
			},
		},
		writerCase{
			name: "App policy", canonical: AppModelInvokePolicyKey,
			alias: strings.ToLower(AppModelInvokePolicyKey), aliasValue: "0", value: "1",
			write: func() error {
				return RunAppPluginTransaction(db, func(tx *gorm.DB) error {
					_, err := PublishAppExecutionPolicyTx(
						tx, AppModelInvokePolicyKey, `{"version":1}`, 1, time.Unix(1, 0),
					)
					return err
				})
			},
		},
		writerCase{
			name: "App rollout", canonical: operation_setting.AppPluginV1EnabledOptionKey,
			alias:      strings.ToLower(operation_setting.AppPluginV1EnabledOptionKey),
			aliasValue: "true", value: "false",
			write: func() error {
				return UpdateOption(operation_setting.AppPluginV1EnabledOptionKey, "false")
			},
		},
	)

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, db.Exec("DELETE FROM app_execution_policy_versions").Error)
			require.NoError(t, db.Exec("DELETE FROM options").Error)
			requestPolicySnapshot.Store(previousSnapshot)
			common.RetryTimes = previousRetryTimes
			*system_setting.GetPasskeySettings() = system_setting.PasskeySettings{
				Origins: "https://example.com,https://login.example.com",
			}
			system_setting.ServerAddress = ""
			common.OptionMapRWMutex.Lock()
			common.OptionMap = maps.Clone(previousSnapshot.Options)
			beforeOptions := maps.Clone(common.OptionMap)
			common.OptionMapRWMutex.Unlock()
			beforeRequestPolicy := CurrentRequestPolicy()
			beforePasskey := system_setting.PasskeySettingsSnapshot()

			require.NoError(t, db.Create(&Option{Key: test.alias, Value: test.aliasValue}).Error)
			var equivalent int64
			require.NoError(t, db.Model(&Option{}).
				Where(clause.Eq{Column: "key", Value: test.canonical}).Count(&equivalent).Error)
			collides := equivalent == 1
			switch db.Dialector.Name() {
			case "sqlite":
				assert.Equal(t, !strings.Contains(test.name, "trailing") &&
					!strings.Contains(test.name, "accent"), collides)
			case "mysql":
				assert.True(t, collides)
			case "postgres":
				assert.False(t, collides)
			}

			err := test.write()
			if collides {
				require.EqualError(t, err, protectedOptionWriteError(test.canonical).Error())
			} else {
				require.NoError(t, err)
			}

			var rows []Option
			require.NoError(t, db.Find(&rows).Error)
			var aliasRows, canonicalRows []Option
			for _, row := range rows {
				if row.Key == test.alias {
					aliasRows = append(aliasRows, row)
				}
				if row.Key == test.canonical {
					canonicalRows = append(canonicalRows, row)
				}
			}
			require.Equal(t, []Option{{Key: test.alias, Value: test.aliasValue}}, aliasRows)
			if collides {
				assert.Empty(t, canonicalRows)
				assert.Same(t, beforeRequestPolicy, CurrentRequestPolicy())
				assert.Equal(t, beforePasskey, system_setting.PasskeySettingsSnapshot())
				common.OptionMapRWMutex.RLock()
				assert.Equal(t, beforeOptions, common.OptionMap)
				common.OptionMapRWMutex.RUnlock()
				var versions int64
				require.NoError(t, db.Model(&AppExecutionPolicyVersion{}).Count(&versions).Error)
				assert.Zero(t, versions)
			} else {
				require.Len(t, canonicalRows, 1)
				assert.Equal(t, test.value, canonicalRows[0].Value)
			}
		})
	}

	t.Run("generic option", func(t *testing.T) {
		require.NoError(t, db.Exec("DELETE FROM app_execution_policy_versions").Error)
		require.NoError(t, db.Exec("DELETE FROM options").Error)
		common.OptionMapRWMutex.Lock()
		common.OptionMap = map[string]string{}
		common.OptionMapRWMutex.Unlock()
		const canonical, alias = "Ordinary.Option", "ordinary.option"
		require.NoError(t, db.Create(&Option{Key: alias, Value: "before"}).Error)
		var equivalent int64
		require.NoError(t, db.Model(&Option{}).
			Where(clause.Eq{Column: "key", Value: canonical}).Count(&equivalent).Error)

		require.NoError(t, UpdateOption(canonical, "after"))

		var rows []Option
		require.NoError(t, db.Find(&rows).Error)
		if equivalent == 1 {
			assert.Equal(t, []Option{{Key: alias, Value: "after"}}, rows)
		} else {
			assert.ElementsMatch(t, []Option{
				{Key: alias, Value: "before"},
				{Key: canonical, Value: "after"},
			}, rows)
		}
		common.OptionMapRWMutex.RLock()
		assert.Equal(t, "after", common.OptionMap[canonical])
		common.OptionMapRWMutex.RUnlock()
	})
}

func TestAppExecutionRolloutOptionsUseLockedDatabaseTruth(t *testing.T) {
	db := useMigrationTestDB(t)
	require.NoError(t, db.AutoMigrate(&Option{}))
	require.NoError(t, db.Create(&[]Option{
		{Key: operation_setting.AppPluginV1EnabledOptionKey, Value: "true"},
		{Key: operation_setting.AppPluginSeedanceEnabledOptionKey, Value: "false"},
		{Key: operation_setting.AppExecutionGrantsEnabledOptionKey, Value: "true"},
	}).Error)

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		allowed, err := AppExecutionRolloutAllowsTx(tx, "ordinary-app")
		require.NoError(t, err)
		assert.True(t, allowed)
		allowed, err = AppExecutionRolloutAllowsTx(tx, "seedance-repro")
		require.NoError(t, err)
		assert.False(t, allowed)
		return nil
	}))
}

func TestSQLiteAppExecutionRolloutCheckLinearizesGrantWithDisableWithoutTriggers(t *testing.T) {
	if dialect := os.Getenv("APP_PLUGIN_TEST_DIALECT"); dialect != "" && dialect != "sqlite" {
		t.Skip("SQLite rollout lock regression")
	}
	db := useMigrationTestDB(t)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(2)
	require.NoError(t, db.AutoMigrate(&Option{}))
	require.NoError(t, db.Create(&[]Option{
		{Key: operation_setting.AppPluginV1EnabledOptionKey, Value: "true"},
		{Key: operation_setting.AppExecutionGrantsEnabledOptionKey, Value: "true"},
	}).Error)
	require.NoError(t, db.Exec(
		"CREATE TABLE rollout_update_audit (marker integer NOT NULL)",
	).Error)
	require.NoError(t, db.Exec(`
CREATE TRIGGER options_rollout_noop_audit
AFTER UPDATE ON options
WHEN OLD.value = NEW.value
BEGIN
  INSERT INTO rollout_update_audit (marker) VALUES (1);
END`).Error)

	type rolloutResult struct {
		allowed bool
		err     error
	}
	checkReady := make(chan rolloutResult, 1)
	transactionDone := make(chan error, 1)
	releaseGrant := make(chan struct{})
	go func() {
		var allowed bool
		transactionDone <- db.Transaction(func(tx *gorm.DB) error {
			var checkErr error
			allowed, checkErr = AppExecutionRolloutAllowsTx(tx, "ordinary-app")
			if checkErr != nil {
				return checkErr
			}
			checkReady <- rolloutResult{allowed: allowed}
			<-releaseGrant
			return nil
		})
	}()

	var result rolloutResult
	select {
	case result = <-checkReady:
	case transactionErr := <-transactionDone:
		result.err = transactionErr
	case <-time.After(5 * time.Second):
		t.Fatal("rollout check did not acquire the SQLite write lock")
	}
	require.NoError(t, result.err)
	require.True(t, result.allowed)

	disableDone := make(chan error, 1)
	go func() {
		disableDone <- db.Model(&Option{}).
			Where(clause.Eq{
				Column: clause.Column{Name: "key"},
				Value:  operation_setting.AppExecutionGrantsEnabledOptionKey,
			}).
			UpdateColumn("value", "false").Error
	}()
	select {
	case disableErr := <-disableDone:
		close(releaseGrant)
		require.NoError(t, disableErr)
		t.Fatal("rollout disable committed before the grant transaction released its lock")
	case <-time.After(200 * time.Millisecond):
	}
	close(releaseGrant)
	require.NoError(t, <-transactionDone)
	require.NoError(t, <-disableDone)

	var enabled string
	require.NoError(t, db.Model(&Option{}).
		Select("value").
		Where(clause.Eq{
			Column: clause.Column{Name: "key"},
			Value:  operation_setting.AppExecutionGrantsEnabledOptionKey,
		}).
		Scan(&enabled).Error)
	assert.Equal(t, "false", enabled)
	var triggerCount int64
	require.NoError(t, db.Table("rollout_update_audit").Count(&triggerCount).Error)
	assert.Zero(t, triggerCount, "rollout checks must not fire row UPDATE triggers")
}

func TestOptionWriterInitializesMissingRowIdempotently(t *testing.T) {
	if dialect := os.Getenv("APP_PLUGIN_TEST_DIALECT"); dialect != "mysql" && dialect != "postgres" {
		t.Skip("requires real row-level locking")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.AutoMigrate(&Option{}))
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = map[string]string{}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})
	const callbackName = "test:concurrent_option_initialization"
	var barrierMu sync.Mutex
	arrivals := 0
	reached := make(chan struct{})
	release := make(chan struct{})
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		option, ok := tx.Statement.Dest.(*Option)
		if !ok || option.Key != "concurrent.initialization" {
			return
		}
		barrierMu.Lock()
		if arrivals >= 2 {
			barrierMu.Unlock()
			return
		}
		arrivals++
		if arrivals == 2 {
			close(reached)
		}
		barrierMu.Unlock()
		<-release
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Create().Remove(callbackName))
	})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			results <- UpdateOption("concurrent.initialization", "same")
		}()
	}
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("writers did not concurrently observe the missing option")
	}
	close(release)

	firstErr := <-results
	secondErr := <-results
	require.NoError(t, firstErr)
	require.NoError(t, secondErr)
	var count int64
	require.NoError(t, db.Model(&Option{}).
		Where(clause.Eq{Column: clause.Column{Name: "key"}, Value: "concurrent.initialization"}).
		Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

func TestProtectedOptionWriterTransactionsRetryMySQLDeadlocks(t *testing.T) {
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "mysql" {
		t.Skip("requires MySQL deadlock retry semantics")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.AutoMigrate(&Option{}, &AppExecutionPolicyVersion{}))
	previousSnapshot := CurrentRequestPolicy()
	previousPasskeySettings := *system_setting.GetPasskeySettings()
	previousServerAddress := system_setting.ServerAddress
	common.OptionMapRWMutex.Lock()
	previousOptions := maps.Clone(common.OptionMap)
	common.OptionMap = maps.Clone(previousSnapshot.Options)
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		requestPolicySnapshot.Store(previousSnapshot)
		*system_setting.GetPasskeySettings() = previousPasskeySettings
		system_setting.ServerAddress = previousServerAddress
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})

	for _, test := range []struct {
		name       string
		key        string
		attemptKey string
		value      string
	}{
		{name: "generic", key: "retry.generic", attemptKey: "retry.generic", value: "final"},
		{name: "Passkey", key: "passkey.rp_id", attemptKey: "ServerAddress", value: "localhost"},
		{name: "Request Policy", key: "RetryTimes", attemptKey: "RetryTimes", value: "3"},
		{name: "App policy", key: AppModelInvokePolicyKey, attemptKey: AppModelInvokePolicyKey, value: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, db.Exec("DELETE FROM app_execution_policy_versions").Error)
			require.NoError(t, db.Exec("DELETE FROM options").Error)

			callbackName := "test:option_writer_deadlock_retry_" + strings.ReplaceAll(test.name, " ", "_")
			createCallbackName := callbackName + "_attempt"
			var createAttempts atomic.Int32
			require.NoError(t, db.Callback().Create().Before("gorm:create").Register(createCallbackName, func(tx *gorm.DB) {
				option, ok := tx.Statement.Dest.(*Option)
				if ok && option.Key == test.attemptKey {
					createAttempts.Add(1)
				}
			}))
			var injected atomic.Bool
			require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
				if tx.Statement.Table != "options" {
					return
				}
				if injected.CompareAndSwap(false, true) {
					tx.AddError(&mysqlDriver.MySQLError{Number: 1213, Message: "injected option writer deadlock"})
				}
			}))
			defer func() {
				require.NoError(t, db.Callback().Update().Remove(callbackName))
				require.NoError(t, db.Callback().Create().Remove(createCallbackName))
			}()

			var version int64
			var err error
			switch test.name {
			case "generic":
				err = UpdateOption(test.key, test.value)
			case "Passkey":
				_, err = UpdatePasskeyDomainOptions(map[string]string{test.key: test.value}, false, "")
			case "Request Policy":
				err = UpdateRequestPolicyOptions(map[string]string{test.key: test.value})
			case "App policy":
				err = RunAppPluginTransaction(db, func(tx *gorm.DB) error {
					var err error
					version, err = PublishAppExecutionPolicyTx(
						tx, test.key, `{"version":1}`, 1, time.Unix(1, 0),
					)
					return err
				})
			}
			require.NoError(t, err)
			assert.True(t, injected.Load())
			assert.GreaterOrEqual(t, createAttempts.Load(), int32(2))

			var rows []Option
			require.NoError(t, db.Where(clause.Eq{Column: "key", Value: test.key}).Find(&rows).Error)
			require.Len(t, rows, 1)
			if test.name == "App policy" {
				assert.Equal(t, int64(1), version)
				var versions int64
				require.NoError(t, db.Model(&AppExecutionPolicyVersion{}).Count(&versions).Error)
				assert.EqualValues(t, 1, versions)
			}
			assert.Equal(t, test.value, rows[0].Value)
		})
	}
}

func TestPasskeyDomainOptionRetryRecomputesFromCurrentDatabaseState(t *testing.T) {
	if dialect := os.Getenv("APP_PLUGIN_TEST_DIALECT"); dialect != "mysql" && dialect != "postgres" {
		t.Skip("requires real database deadlock retry semantics")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.AutoMigrate(&Option{}, &PasskeyCredential{}))
	const (
		initialOrigins    = "https://old.example.com,https://new.example.com"
		concurrentOrigins = "https://old.example.com,https://new.example.com,https://concurrent.example.com"
	)
	initial := map[string]string{
		"ServerAddress":         "",
		"passkey.legacy_rp_ids": "",
		"passkey.origins":       initialOrigins,
		"passkey.rp_id":         "old.example.com",
	}
	keys := slices.Sorted(maps.Keys(initial))
	for _, key := range keys {
		require.NoError(t, db.Create(&Option{Key: key, Value: initial[key]}).Error)
	}

	previousSettings := *system_setting.GetPasskeySettings()
	previousServerAddress := system_setting.ServerAddress
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = maps.Clone(initial)
	common.OptionMapRWMutex.Unlock()
	*system_setting.GetPasskeySettings() = system_setting.PasskeySettings{
		RPID: "old.example.com", Origins: initialOrigins,
	}
	system_setting.ServerAddress = ""
	t.Cleanup(func() {
		*system_setting.GetPasskeySettings() = previousSettings
		system_setting.ServerAddress = previousServerAddress
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})

	const (
		updateCallback = "test:passkey_retry_mutated_input_deadlock"
		queryCallback  = "test:passkey_retry_concurrent_commit"
	)
	var injected, concurrentApplied atomic.Bool
	var concurrentErr error
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(updateCallback, func(tx *gorm.DB) {
		if tx.Statement.Table != "options" || !injected.CompareAndSwap(false, true) {
			return
		}
		if db.Dialector.Name() == "mysql" {
			tx.AddError(&mysqlDriver.MySQLError{Number: 1213, Message: "injected Passkey deadlock"})
		} else {
			tx.AddError(&pgconn.PgError{Code: "40P01", Message: "injected Passkey deadlock"})
		}
	}))
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(queryCallback, func(tx *gorm.DB) {
		if tx.Statement.Table != "options" || !injected.Load() ||
			!concurrentApplied.CompareAndSwap(false, true) {
			return
		}
		concurrentErr = db.Model(&Option{}).
			Where(clause.Eq{Column: clause.Column{Name: "key"}, Value: "passkey.origins"}).
			UpdateColumn("value", concurrentOrigins).Error
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Update().Remove(updateCallback))
		require.NoError(t, db.Callback().Query().Remove(queryCallback))
	})

	requested := map[string]string{"passkey.rp_id": "new.example.com"}
	change, err := UpdatePasskeyDomainOptions(requested, false, "")

	require.NoError(t, err)
	require.NoError(t, concurrentErr)
	require.True(t, injected.Load())
	require.True(t, concurrentApplied.Load())
	assert.Equal(t, map[string]string{"passkey.rp_id": "new.example.com"}, requested)
	require.NotNil(t, change)
	assert.Equal(t, concurrentOrigins, change.Origins)
	var stored Option
	require.NoError(t, db.Where(clause.Eq{Column: "key", Value: "passkey.origins"}).
		First(&stored).Error)
	assert.Equal(t, concurrentOrigins, stored.Value)
	assert.Equal(t, concurrentOrigins, system_setting.PasskeySettingsSnapshot().Origins)
}

func TestAppExecutionRolloutReadDoesNotInitializeMissingExtendedRows(t *testing.T) {
	db := useMigrationTestDB(t)
	require.NoError(t, db.Table("options").Migrator().CreateTable(&extendedRequiredOption{}))
	require.NoError(t, db.Table("options").Create(&[]extendedRequiredOption{
		{
			Key:          operation_setting.AppPluginV1EnabledOptionKey,
			Value:        "true",
			SchemaMarker: "master",
		},
		{
			Key:          operation_setting.AppExecutionGrantsEnabledOptionKey,
			Value:        "true",
			SchemaMarker: "grants",
		},
	}).Error)

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		allowed, err := AppExecutionRolloutAllowsTx(tx, "ordinary-app")
		require.NoError(t, err)
		assert.True(t, allowed)
		allowed, err = AppExecutionRolloutAllowsTx(tx, "seedance-repro")
		require.NoError(t, err)
		assert.False(t, allowed)
		return nil
	}))
	var rowCount int64
	require.NoError(t, db.Table("options").Count(&rowCount).Error)
	assert.EqualValues(t, 2, rowCount)
	var seedanceRows int64
	require.NoError(t, db.Table("options").
		Where(clause.Eq{Column: clause.Column{Name: "key"}, Value: operation_setting.AppPluginSeedanceEnabledOptionKey}).
		Count(&seedanceRows).Error)
	assert.Zero(t, seedanceRows)
}

func TestAppExecutionRolloutSharedReadsDoNotSerialize(t *testing.T) {
	if dialect := os.Getenv("APP_PLUGIN_TEST_DIALECT"); dialect != "mysql" && dialect != "postgres" {
		t.Skip("requires real shared row locks")
	}
	db := useMigrationTestDB(t)
	require.NoError(t, db.AutoMigrate(&Option{}))
	require.NoError(t, db.Create(&[]Option{
		{Key: operation_setting.AppPluginV1EnabledOptionKey, Value: "true"},
		{Key: operation_setting.AppExecutionGrantsEnabledOptionKey, Value: "true"},
	}).Error)

	for round := range 3 {
		t.Run(fmt.Sprintf("round-%d", round+1), func(t *testing.T) {
			firstLocked := make(chan struct{})
			releaseFirst := make(chan struct{})
			firstDone := make(chan error, 1)
			go func() {
				firstDone <- db.Transaction(func(tx *gorm.DB) error {
					allowed, err := AppExecutionRolloutAllowsTx(tx, "ordinary-app")
					if err != nil {
						return err
					}
					if !allowed {
						return fmt.Errorf("first rollout read was disabled")
					}
					close(firstLocked)
					<-releaseFirst
					return nil
				})
			}()
			select {
			case <-firstLocked:
			case <-time.After(5 * time.Second):
				t.Fatal("first rollout reader did not acquire its locks")
			}

			secondDone := make(chan error, 1)
			go func() {
				secondDone <- db.Transaction(func(tx *gorm.DB) error {
					allowed, err := AppExecutionRolloutAllowsTx(tx, "ordinary-app")
					if err != nil {
						return err
					}
					if !allowed {
						return fmt.Errorf("second rollout read was disabled")
					}
					return nil
				})
			}()
			var secondErr error
			secondCompletedWhileFirstHeld := false
			select {
			case secondErr = <-secondDone:
				secondCompletedWhileFirstHeld = true
			case <-time.After(time.Second):
			}
			close(releaseFirst)
			require.NoError(t, <-firstDone)
			if !secondCompletedWhileFirstHeld {
				secondErr = <-secondDone
			}
			require.NoError(t, secondErr)
			assert.True(t, secondCompletedWhileFirstHeld,
				"shared rollout reads must not serialize unrelated app traffic")
		})
	}
}

func TestRequestPolicyBulkWriterUsesProtectedTransaction(t *testing.T) {
	db := useMigrationTestDB(t)
	require.NoError(t, db.AutoMigrate(&Option{}))
	previousSnapshot := CurrentRequestPolicy()
	previousRetryTimes := common.RetryTimes
	common.OptionMapRWMutex.Lock()
	previousOptions := maps.Clone(common.OptionMap)
	common.OptionMap = maps.Clone(previousSnapshot.Options)
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		requestPolicySnapshot.Store(previousSnapshot)
		common.RetryTimes = previousRetryTimes
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})

	require.NoError(t, UpdateOptionsBulk(map[string]string{
		"RetryTimes":      "3",
		"ordinary.option": "preserved",
	}))
	assert.Equal(t, 3, CurrentRequestPolicy().RetryTimes)
	for key, value := range map[string]string{
		"RetryTimes":      "3",
		"ordinary.option": "preserved",
	} {
		var option Option
		require.NoError(t, db.Where(clause.Eq{Column: "key", Value: key}).First(&option).Error)
		assert.Equal(t, value, option.Value)
	}
}

func TestPrefillIndexRenameMigrationSecondRunIsIdempotent(t *testing.T) {
	db := useMigrationTestDB(t)
	if db.Dialector.Name() != "postgres" {
		t.Skip("PostgreSQL-specific index rename")
	}
	require.NoError(t, db.AutoMigrate(&PrefillGroup{}))
	require.NoError(t, db.Migrator().DropIndex(&PrefillGroup{}, prefillGroupNameIndex))
	require.NoError(t, db.Exec(
		"CREATE UNIQUE INDEX ? ON ? (?)",
		clause.Column{Name: "idx_prefill_groups_name"},
		clause.Table{Name: "prefill_groups"},
		clause.Column{Name: "name"},
	).Error)

	require.NoError(t, migratePrefillGroupUniqueness(db))
	require.NoError(t, db.AutoMigrate(&PrefillGroup{}))
	recorder := &migrationSQLRecorder{}
	second := db.Session(&gorm.Session{Logger: recorder})
	require.NoError(t, migratePrefillGroupUniqueness(second))
	require.NoError(t, second.AutoMigrate(&PrefillGroup{}))
	assert.Empty(t, recorder.schemaMutations())
	assert.True(t, db.Migrator().HasIndex(&PrefillGroup{}, prefillGroupNameIndex))
}

func TestChannelTypeCompatibilityIDsRemainStable(t *testing.T) {
	assert.Equal(t, 62, constant.ChannelTypeVolcEngine3D)
	assert.Equal(t, "VolcEngine3D", constant.GetChannelTypeName(62))
	assert.Equal(t, 63, constant.ChannelTypeDummy)
	assert.Equal(t, 64, constant.ChannelTypeVLLM)
	assert.Equal(t, 65, constant.ChannelTypeSGLang)
	assert.Equal(t, constant.ChannelTypeSGLang, constant.ChannelTypeMax)
}

func migrationTableModels() []any {
	return []any{
		&Channel{}, &Token{}, &User{}, &UserSession{}, &AuthFlow{},
		&ExternalIdentityClaim{}, &PasskeyCredential{}, &Option{}, &LoginEncryptionKey{},
		&Redemption{}, &Ability{}, &Log{}, &Midjourney{}, &TopUp{}, &QuotaData{},
		&Task{}, &TaskPlugin{}, &Model{}, &Vendor{}, &PrefillGroup{}, &Setup{},
		&TwoFA{}, &TwoFABackupCode{}, &Checkin{}, &SubscriptionOrder{},
		&UserSubscription{}, &SubscriptionPreConsumeRecord{}, &CustomOAuthProvider{},
		&UserOAuthBinding{}, &PerfMetric{}, &SystemInstance{}, &SystemTask{},
		&SystemTaskLock{}, &StreamExecution{}, &APIFile{}, &Batch{}, &BatchItem{},
		&UpstreamSource{}, &UpstreamGroup{}, &UpstreamManagedRoute{},
		&UpstreamMetricSnapshot{}, &UpstreamPriceEvidence{}, &UpstreamSyncDevice{},
		&UpstreamSyncBatch{}, &UpstreamSyncCommand{}, &CasbinRule{}, &AuthzRole{},
		&SubscriptionPlan{},
		&AppVersion{}, &AppInstallation{}, &AppInstallationIdempotency{},
		&AppRouteClaim{}, &AppServiceCredential{}, &AppEntitlementPolicy{},
		&AppServiceCredentialBinding{}, &AppPluginLaunchCode{}, &AppPluginSession{},
		&AppPluginExchangeReplay{}, &AppPluginSessionRevokeReplay{},
		&AppExecutionPolicyVersion{}, &AppPassthroughRuleVersion{},
		&AppExecutionGrant{}, &AppTaskExecution{}, &AppResponseResult{},
		&AppTaskReconcile{}, &AppTaskSettlement{}, &AppTaskOutbox{},
		&AppTaskCancelReplay{},
	}
}

func assertMigrationTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, table := range migrationTableModels() {
		assert.True(t, db.Migrator().HasTable(table), fmt.Sprintf("%T", table))
	}
}

func TestMigrationFreshUpgradeAndSecondRunPreserveProductionAndB16Tables(t *testing.T) {
	t.Run("fresh and second", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, migrateDB())
		assertMigrationTables(t, db)
		require.NoError(t, migrateDB())
		assertMigrationTables(t, db)
	})

	t.Run("upgrade and second", func(t *testing.T) {
		db := useMigrationTestDB(t)
		require.NoError(t, MigrateAppPluginTables(db))
		require.NoError(t, MigrateAppPluginLaunchTables(db))
		require.NoError(t, MigrateAppExecutionTables(db))
		require.NoError(t, db.Create(&AppPassthroughRuleVersion{
			IdentityHash: "legacy-rule", SchemaSHA256: "legacy-schema",
		}).Error)
		require.NoError(t, db.Table("legacy_operator_notes").Migrator().
			CreateTable(&legacyMigrationSentinel{}))
		require.NoError(t, db.Table("legacy_operator_notes").
			Create(&legacyMigrationSentinel{ID: 1, Marker: "preserved"}).Error)

		require.NoError(t, migrateDB())
		assertMigrationTables(t, db)
		require.NoError(t, migrateDB())
		assertMigrationTables(t, db)
		assert.True(t, db.Migrator().HasTable("legacy_operator_notes"))

		var rule AppPassthroughRuleVersion
		require.NoError(t, db.Where("identity_hash = ?", "legacy-rule").First(&rule).Error)
		assert.Equal(t, "legacy-schema", rule.SchemaSHA256)
		var sentinel legacyMigrationSentinel
		require.NoError(t, db.Table("legacy_operator_notes").First(&sentinel, 1).Error)
		assert.Equal(t, "preserved", sentinel.Marker)
	})
}
