package model

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	optionPrimaryKeyTmpTable    = "options_pk_tmp"
	optionPrimaryKeyLockName    = "new_api_options_pk"
	optionPrimaryKeyLockID      = 75820193
	optionLegacyTablePrefix     = "options_legacy_"
	optionMigrationMaxRows      = 10_000
	optionDigestKeySize         = 32
	optionArtifactIDSize        = 16
	optionInsertTrigger         = "options_pk_migrate_insert"
	optionUpdateTrigger         = "options_pk_migrate_update"
	optionDeleteTrigger         = "options_pk_migrate_delete"
	optionMySQLTmpPrefix        = "options_pk_tmp_"
	optionInsertTriggerPrefix   = "options_pk_migrate_ai_"
	optionUpdateTriggerPrefix   = "options_pk_migrate_au_"
	optionDeleteTriggerPrefix   = "options_pk_migrate_ad_"
	optionMigrationMarker       = "new-api/options-pk-migration/v1"
	optionMigrationMarkerCol    = "_new_api_options_pk_owner_v1"
	optionMigrationStateMarker  = "new-api/options-pk-migration-state/v1"
	optionMigrationStateCol     = "_new_api_options_pk_state_v1"
	optionMigrationPrepared     = "prepared"
	optionMigrationValidated    = "validated"
	optionMigrationInsertGone   = "insert_removed"
	optionMigrationUpdateGone   = "update_removed"
	optionMigrationBridgesGone  = "bridges_removed"
	optionForwardIntentPrefix   = "forward/"
	optionForwardDropInsert     = "drop_insert"
	optionForwardDropUpdate     = "drop_update"
	optionForwardDropDelete     = "drop_delete"
	optionRestoreIntentPrefix   = "restore/"
	optionRestoreCreateDelete   = "create_delete"
	optionRestoreCreateUpdate   = "create_update"
	optionRestoreCreateInsert   = "create_insert"
	optionRestoreClear          = "clear"
	optionMigrationStateSize    = 160
	optionMigrationMaxOrphans   = 32
	optionArtifactIDAttempts    = 8
	optionTerminalJournalTable  = "_new_api_options_pk_terminal_v1"
	optionTerminalJournalMark   = "new-api/options-pk-terminal/v1"
	optionTerminalJournalOwner  = "new-api/options-pk-terminal-owner/v1"
	optionTerminalActiveRows    = "new-api/options-pk-terminal-active-rows/v1"
	optionTerminalRetainedRows  = "new-api/options-pk-terminal-retained-rows/v1"
	optionTerminalCanonicalRows = "new-api/options-pk-terminal-retained-canonical-rows/v1"
)

var (
	errMySQLMetadataVisibilityInspection = errors.New(
		"inspect MySQL metadata visibility capability",
	)
	errMySQLGlobalMetadataVisibilityRequired = errors.New(
		"MySQL options migration requires global table metadata visibility",
	)
	errMySQLOptionFinalizationRecoveryBlocked = errors.New(
		"MySQL options migration recovery blocked",
	)
	errMySQLOptionTerminalJournalInspection = errors.New(
		"inspect MySQL options completion journal",
	)
	errMySQLOptionTerminalJournalRecoveryBlocked = errors.New(
		"MySQL options completion journal recovery blocked",
	)
)

type optionMigrationRow struct {
	Key            string
	Value          sql.NullString
	EquivalenceKey []byte `gorm:"column:equivalence_key"`
}

type mysqlOptionColumn struct {
	Name  string `gorm:"column:COLUMN_NAME"`
	Extra string `gorm:"column:EXTRA"`
}

type mysqlOptionTrigger struct {
	Name      string `gorm:"column:trigger_name"`
	Event     string `gorm:"column:event_manipulation"`
	Timing    string `gorm:"column:action_timing"`
	TableName string `gorm:"column:event_object_table"`
	Action    string `gorm:"column:action_statement"`
}

type mysqlOptionTriggerSpec struct {
	Name   string
	Event  string
	Action string
}

type mysqlOptionMigrationArtifacts struct {
	ID                 string
	Marker             string
	TmpTable           string
	BackupTable        string
	SourceSchemaSHA256 string
	ActiveSchemaSHA256 string
	InsertTrigger      string
	UpdateTrigger      string
	DeleteTrigger      string
}

type mysqlOptionMigrationState struct {
	Phase              string
	SourceSchemaSHA256 string
	ActiveSchemaSHA256 string
}

type mysqlOptionTerminalJournal struct {
	ID                          string
	Phase                       string
	SourceSchemaSHA256          string
	TerminalSchemaSHA256        string
	ActiveRowsSHA256            string
	RetainedRowsSHA256          string
	RetainedCanonicalRowsSHA256 string
}

type mysqlOptionTerminalJournalColumn struct {
	Name         string         `gorm:"column:column_name"`
	DataType     string         `gorm:"column:data_type"`
	ColumnType   string         `gorm:"column:column_type"`
	Nullable     string         `gorm:"column:is_nullable"`
	DefaultValue sql.NullString `gorm:"column:column_default"`
	Extra        string         `gorm:"column:extra"`
	Comment      string         `gorm:"column:column_comment"`
	CharacterSet sql.NullString `gorm:"column:character_set_name"`
	Collation    sql.NullString `gorm:"column:collation_name"`
}

type mysqlOptionTerminalJournalSnapshot struct {
	Singleton                   uint8  `gorm:"column:singleton"`
	ActiveRowsSHA256            string `gorm:"column:active_rows_sha256"`
	RetainedRowsSHA256          string `gorm:"column:retained_rows_sha256"`
	RetainedCanonicalRowsSHA256 string `gorm:"column:retained_canonical_rows_sha256"`
}

type mysqlOptionFinalizationRestorePoint struct {
	StatePhase           string
	BridgePhase          string
	MarkersDropped       bool
	TerminalSchemaSHA256 string
}

type mysqlOptionRestoreIntent struct {
	TargetPhase string
	Operation   string
}

type mysqlOptionForwardIntent struct {
	TargetPhase string
	Operation   string
}

func (state mysqlOptionMigrationState) value() string {
	return state.Phase + ":" + state.SourceSchemaSHA256 + ":" + state.ActiveSchemaSHA256
}

func (journal mysqlOptionTerminalJournal) value() string {
	return strings.Join([]string{
		optionTerminalJournalMark,
		journal.ID,
		journal.Phase,
		journal.SourceSchemaSHA256,
		journal.TerminalSchemaSHA256,
	}, ":")
}

func (journal mysqlOptionTerminalJournal) hasSnapshot() bool {
	return journal.ActiveRowsSHA256 != "" &&
		journal.RetainedRowsSHA256 != "" &&
		journal.RetainedCanonicalRowsSHA256 != ""
}

func mysqlOptionMigrationPhaseCode(phase string) string {
	switch phase {
	case optionMigrationPrepared:
		return "p"
	case optionMigrationValidated:
		return "v"
	case optionMigrationInsertGone:
		return "i"
	case optionMigrationUpdateGone:
		return "u"
	case optionMigrationBridgesGone:
		return "b"
	default:
		return ""
	}
}

func mysqlOptionMigrationPhaseFromCode(code string) string {
	switch code {
	case "p":
		return optionMigrationPrepared
	case "v":
		return optionMigrationValidated
	case "i":
		return optionMigrationInsertGone
	case "u":
		return optionMigrationUpdateGone
	case "b":
		return optionMigrationBridgesGone
	default:
		return ""
	}
}

func (intent mysqlOptionForwardIntent) phase() string {
	return optionForwardIntentPrefix +
		mysqlOptionMigrationPhaseCode(intent.TargetPhase) + "/" + intent.Operation
}

func (intent mysqlOptionRestoreIntent) phase() string {
	return optionRestoreIntentPrefix +
		mysqlOptionMigrationPhaseCode(intent.TargetPhase) + "/" + intent.Operation
}

type postgresOptionColumn struct {
	Name             string `gorm:"column:column_name"`
	IsIdentity       string `gorm:"column:is_identity"`
	IsGenerated      string `gorm:"column:is_generated"`
	HasOwnedSequence bool   `gorm:"column:has_owned_sequence"`
}

type postgresOptionMetadata struct {
	HasUnsupportedRelationMetadata bool `gorm:"column:has_unsupported_relation_metadata"`
	HasForeignKey                  bool `gorm:"column:has_foreign_key"`
	HasTrigger                     bool `gorm:"column:has_trigger"`
	HasRLS                         bool `gorm:"column:has_rls"`
	HasRule                        bool `gorm:"column:has_rule"`
	HasDependentView               bool `gorm:"column:has_dependent_view"`
	HasDescription                 bool `gorm:"column:has_description"`
	HasACL                         bool `gorm:"column:has_acl"`
	HasColumnACL                   bool `gorm:"column:has_column_acl"`
	HasOtherOwner                  bool `gorm:"column:has_other_owner"`
	HasPublication                 bool `gorm:"column:has_publication"`
}

func (metadata postgresOptionMetadata) hasUnsupportedSchema() bool {
	return metadata.HasUnsupportedRelationMetadata || metadata.HasForeignKey ||
		metadata.HasTrigger || metadata.HasRLS || metadata.HasRule ||
		metadata.HasDependentView || metadata.HasDescription || metadata.HasACL ||
		metadata.HasColumnACL || metadata.HasOtherOwner || metadata.HasPublication
}

type sqliteOptionColumn struct {
	Name         string         `gorm:"column:name"`
	Type         string         `gorm:"column:type"`
	NotNull      int            `gorm:"column:not_null"`
	DefaultValue sql.NullString `gorm:"column:default_value"`
	PrimaryKey   int            `gorm:"column:primary_key"`
	Hidden       int            `gorm:"column:hidden"`
}

type sqliteOptionIndex struct {
	Name    string `gorm:"column:name"`
	Unique  int    `gorm:"column:unique"`
	Origin  string `gorm:"column:origin"`
	Partial int    `gorm:"column:partial"`
}

type sqliteOptionIndexColumn struct {
	Name       sql.NullString `gorm:"column:name"`
	Descending int            `gorm:"column:descending"`
	Collation  string         `gorm:"column:collation"`
}

type sqliteOptionMigrationSchema struct {
	KeyType      string
	ValueNotNull bool
}

func migrateOptionPrimaryKey(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("migrate options primary key: database is nil")
	}
	if db.Dialector.Name() == "mysql" {
		return withOptionPrimaryKeyLock(db, func(locked *gorm.DB) error {
			completed, err := recoverMySQLOptionTerminalJournal(locked)
			if err != nil {
				return mysqlOptionTerminalJournalRecoveryBlocked(err)
			}
			if completed {
				return nil
			}
			hasOptions, err := mysqlOptionTableExists(locked)
			if err != nil {
				return err
			}
			if !hasOptions {
				return nil
			}
			recovered, err := recoverActiveMySQLOptionMigration(locked)
			if err != nil {
				return err
			}
			if recovered {
				return cleanupMySQLOptionMigrationArtifacts(locked)
			}
			primary, err := optionsKeyIsPrimary(locked)
			if err != nil {
				return err
			}
			if primary {
				return cleanupMySQLOptionMigrationArtifacts(locked)
			}
			if err := validateMySQLGlobalTableMetadataVisibility(locked); err != nil {
				return err
			}
			if err := cleanupMySQLOptionMigrationArtifacts(locked); err != nil {
				return err
			}
			return repairOptionPrimaryKey(locked, rand.Reader)
		})
	}
	if !db.Migrator().HasTable(&Option{}) {
		return nil
	}
	primary, err := optionsKeyIsPrimary(db)
	if err != nil {
		return err
	}
	if primary {
		return nil
	}
	return withOptionPrimaryKeyLock(db, func(locked *gorm.DB) error {
		primary, err := optionsKeyIsPrimary(locked)
		if err != nil {
			return err
		}
		if primary {
			return nil
		}
		return repairOptionPrimaryKey(locked, rand.Reader)
	})
}

func mysqlOptionTableExists(db *gorm.DB) (bool, error) {
	var count int64
	if err := db.Raw(`
SELECT count(*)
FROM information_schema.tables
WHERE table_schema = DATABASE()
  AND table_name = 'options'
  AND table_type = 'BASE TABLE'`).Scan(&count).Error; err != nil {
		return false, fmt.Errorf("inspect MySQL options table")
	}
	return count == 1, nil
}

func optionsKeyIsPrimary(db *gorm.DB) (bool, error) {
	if db.Dialector.Name() == "sqlite" {
		var columns []sqliteOptionColumn
		if err := db.Raw(`
SELECT name, type, "notnull" AS not_null, dflt_value AS default_value,
       pk AS primary_key, hidden
FROM pragma_table_xinfo(?)
ORDER BY cid`, "options").Scan(&columns).Error; err != nil {
			return false, fmt.Errorf("inspect options columns: %w", err)
		}
		primaryColumns := 0
		keyIsNotNullPrimary := false
		for _, column := range columns {
			if column.PrimaryKey == 0 {
				continue
			}
			primaryColumns++
			keyIsNotNullPrimary = keyIsNotNullPrimary ||
				column.Name == "key" && column.NotNull != 0
		}
		return primaryColumns == 1 && keyIsNotNullPrimary, nil
	}
	indexes, err := db.Migrator().GetIndexes(&Option{})
	if err != nil {
		return false, fmt.Errorf("inspect options indexes: %w", err)
	}
	for _, index := range indexes {
		if len(index.Columns()) != 1 || index.Columns()[0] != "key" {
			continue
		}
		if primary, ok := index.PrimaryKey(); ok && primary {
			return true, nil
		}
	}
	if db.Dialector.Name() != "postgres" {
		return false, nil
	}
	// PostgreSQL GetIndexes omits constraint-backed primary keys.
	var count int64
	if err := db.Raw(`
SELECT count(*)
FROM pg_catalog.pg_constraint AS constraint_meta
JOIN pg_catalog.pg_attribute AS attribute_meta
  ON attribute_meta.attrelid = constraint_meta.conrelid
 AND attribute_meta.attnum = constraint_meta.conkey[1]
WHERE constraint_meta.conrelid = to_regclass('options')
  AND constraint_meta.contype = 'p'
  AND cardinality(constraint_meta.conkey) = 1
  AND attribute_meta.attname = 'key'`).Scan(&count).Error; err != nil {
		return false, fmt.Errorf("inspect options constraints: %w", err)
	}
	return count > 0, nil
}

func withOptionPrimaryKeyLock(db *gorm.DB, fn func(*gorm.DB) error) error {
	switch db.Dialector.Name() {
	case "mysql":
		return withMySQLOptionPrimaryKeyLock(db, fn)
	case "postgres":
		return db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", optionPrimaryKeyLockID).Error; err != nil {
				return fmt.Errorf("lock options table: %w", err)
			}
			if err := tx.Exec("LOCK TABLE options IN ACCESS EXCLUSIVE MODE").Error; err != nil {
				return fmt.Errorf("lock options table: %w", err)
			}
			return fn(tx)
		})
	default:
		return db.Transaction(func(tx *gorm.DB) error {
			// A no-op write upgrades SQLite's deferred transaction before the
			// source snapshot, so other writers wait until the swap commits.
			if err := tx.Exec("UPDATE options SET value = value WHERE 0").Error; err != nil {
				return fmt.Errorf("lock options table: %w", err)
			}
			return fn(tx)
		})
	}
}

func withMySQLOptionPrimaryKeyLock(db *gorm.DB, fn func(*gorm.DB) error) (resultErr error) {
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("lock options table: %w", err)
	}
	ctx := db.Statement.Context
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("lock options table: %w", err)
	}
	discard := false
	acquired := false
	completed := false
	locked := db.Session(&gorm.Session{Context: ctx, NewDB: true})
	locked.Statement.ConnPool = conn
	defer func() {
		if acquired {
			cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelCleanup()
			cleanupDB := db.Session(&gorm.Session{Context: cleanupCtx, NewDB: true})
			cleanupDB.Statement.ConnPool = conn
			if _, unlockErr := conn.ExecContext(cleanupCtx, "UNLOCK TABLES"); unlockErr != nil {
				discard = true
				resultErr = errors.Join(resultErr, fmt.Errorf("unlock options tables: %w", unlockErr))
			}
			capabilityFailed := errors.Is(resultErr, errMySQLMetadataVisibilityInspection) ||
				errors.Is(resultErr, errMySQLGlobalMetadataVisibilityRequired) ||
				errors.Is(resultErr, errMySQLOptionTerminalJournalRecoveryBlocked)
			if (resultErr != nil || !completed) && !capabilityFailed {
				if cleanupErr := cleanupMySQLOptionMigrationArtifacts(cleanupDB); cleanupErr != nil {
					discard = true
					resultErr = errors.Join(resultErr, cleanupErr)
				}
			}
			var released sql.NullInt64
			if releaseErr := conn.QueryRowContext(
				cleanupCtx, "SELECT RELEASE_LOCK(?)", optionPrimaryKeyLockName,
			).Scan(&released); releaseErr != nil {
				discard = true
				resultErr = errors.Join(resultErr, fmt.Errorf("release options migration lock: %w", releaseErr))
			} else if !released.Valid || released.Int64 != 1 {
				discard = true
				resultErr = errors.Join(resultErr, fmt.Errorf("release options migration lock: lock was not held"))
			}
		}
		if discard {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}()

	var lockResult int
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 60)", optionPrimaryKeyLockName).Scan(&lockResult); err != nil {
		return fmt.Errorf("lock options table: %w", err)
	}
	if lockResult != 1 {
		return fmt.Errorf("lock options table: timeout")
	}
	acquired = true
	resultErr = fn(locked)
	completed = true
	return resultErr
}

func repairOptionPrimaryKey(db *gorm.DB, entropy io.Reader) error {
	digestKey := make([]byte, optionDigestKeySize)
	if _, err := io.ReadFull(entropy, digestKey); err != nil {
		return fmt.Errorf("generate options conflict digest key: %w", err)
	}
	defer clear(digestKey)

	if db.Dialector.Name() == "mysql" {
		artifacts, err := newAvailableMySQLOptionMigrationArtifacts(db, entropy)
		if err != nil {
			return err
		}
		return repairMySQLOptionPrimaryKey(db, digestKey, artifacts)
	}
	return repairTransactionalOptionPrimaryKey(db, digestKey)
}

func repairTransactionalOptionPrimaryKey(db *gorm.DB, digestKey []byte) error {
	var sourceRowCount int64
	if err := db.Table("options").Count(&sourceRowCount).Error; err != nil {
		return fmt.Errorf("count options rows: table=options")
	}
	if sourceRowCount > optionMigrationMaxRows {
		return fmt.Errorf(
			"read options rows: options row limit exceeded: table=options rows=%d maximum=%d",
			sourceRowCount, optionMigrationMaxRows,
		)
	}

	rows, err := readOptionMigrationRows(db, sourceRowCount)
	if err != nil {
		return fmt.Errorf("read options rows: %w", err)
	}
	deduped, err := dedupeOptionRows(rows, digestKey)
	if err != nil {
		return err
	}
	var postgresColumns []string
	var sqliteSchema sqliteOptionMigrationSchema
	if db.Dialector.Name() == "postgres" {
		postgresColumns, err = inspectPostgresOptionMigrationSchema(db, int64(len(deduped)) != sourceRowCount)
		if err != nil {
			return err
		}
	} else {
		sqliteSchema, err = validateSQLiteOptionMigrationSchema(db)
		if err != nil {
			return err
		}
	}
	if db.Migrator().HasTable(optionPrimaryKeyTmpTable) {
		return fmt.Errorf("options migration artifact collision: table=%s", optionPrimaryKeyTmpTable)
	}
	if err := createTransactionalOptionReplacement(db, sqliteSchema); err != nil {
		return fmt.Errorf("create %s: %w", optionPrimaryKeyTmpTable, err)
	}
	tmpCreated := true
	defer func() {
		if tmpCreated && db.Migrator().HasTable(optionPrimaryKeyTmpTable) {
			_ = db.Migrator().DropTable(optionPrimaryKeyTmpTable)
		}
	}()
	if db.Dialector.Name() == "postgres" {
		if err := validatePostgresOptionReplacementMetadata(db); err != nil {
			return err
		}
	}
	if len(deduped) > 0 {
		if db.Dialector.Name() == "postgres" && int64(len(deduped)) == sourceRowCount {
			if err := copyPostgresOptionRows(db, postgresColumns); err != nil {
				return err
			}
		} else {
			if err := db.Table(optionPrimaryKeyTmpTable).CreateInBatches(deduped, 100).Error; err != nil {
				return fmt.Errorf("insert rebuilt options: %w", err)
			}
		}
	}
	var written int64
	if err := db.Table(optionPrimaryKeyTmpTable).Count(&written).Error; err != nil {
		return fmt.Errorf("count rebuilt options: %w", err)
	}
	if int(written) != len(deduped) {
		return fmt.Errorf("options rebuild wrote %d rows, want %d", written, len(deduped))
	}
	backup := fmt.Sprintf("%s%d", optionLegacyTablePrefix, time.Now().UnixNano())
	if err := swapOptionTables(db, optionPrimaryKeyTmpTable, backup); err != nil {
		return err
	}
	tmpCreated = false
	primary, err := optionsKeyIsPrimary(db)
	if err != nil {
		return err
	}
	if !primary {
		return fmt.Errorf("options table still has no primary key after rebuild")
	}
	common.SysLog(fmt.Sprintf("rebuilt options table with primary key from %d rows into %d keys; previous rows kept in %s", len(rows), len(deduped), backup))
	return nil
}

func inspectPostgresOptionMigrationSchema(db *gorm.DB, hasDuplicates bool) ([]string, error) {
	var features postgresOptionMetadata
	var serverVersion int
	if err := db.Raw("SHOW server_version_num").Scan(&serverVersion).Error; err != nil {
		return nil, fmt.Errorf("inspect PostgreSQL options schema metadata: %w", err)
	}
	if err := db.Raw(postgresOptionMetadataQuery("options", serverVersion)).Scan(&features).Error; err != nil {
		return nil, fmt.Errorf("inspect PostgreSQL options schema metadata: %w", err)
	}
	if features.hasUnsupportedSchema() {
		return nil, fmt.Errorf("PostgreSQL options schema cannot be preserved safely")
	}

	var metadata []postgresOptionColumn
	if err := db.Raw(postgresOptionColumnMetadataQuery(serverVersion), "options").Scan(&metadata).Error; err != nil {
		return nil, fmt.Errorf("inspect PostgreSQL options columns: %w", err)
	}
	columns := make([]string, 0, len(metadata))
	hasKey, hasValue := false, false
	for _, column := range metadata {
		if !optionSafeIdent(column.Name) {
			return nil, fmt.Errorf("PostgreSQL options schema contains an unsafe column identifier")
		}
		if column.IsIdentity == "YES" || column.IsGenerated != "NEVER" {
			return nil, fmt.Errorf("PostgreSQL options generated or identity columns cannot be migrated safely")
		}
		if column.HasOwnedSequence {
			return nil, fmt.Errorf("PostgreSQL options schema cannot be preserved safely")
		}
		columns = append(columns, column.Name)
		hasKey = hasKey || column.Name == "key"
		hasValue = hasValue || column.Name == "value"
	}
	if !hasKey || !hasValue {
		return nil, fmt.Errorf("PostgreSQL options source schema must contain key and value columns")
	}
	if len(columns) > 2 {
		var equivalentDuplicates bool
		if err := db.Raw(`
SELECT EXISTS (
  SELECT 1
  FROM options
  GROUP BY key
  HAVING count(*) > 1
)`).Scan(&equivalentDuplicates).Error; err != nil {
			return nil, fmt.Errorf("inspect PostgreSQL options duplicate keys: %w", err)
		}
		if hasDuplicates || equivalentDuplicates {
			return nil, fmt.Errorf("PostgreSQL options extended schema with duplicate keys cannot be migrated safely")
		}
	}
	return columns, nil
}

func postgresOptionColumnMetadataQuery(serverVersion int) string {
	projection := "column_name, 'NO' AS is_identity, 'NEVER' AS is_generated"
	if serverVersion >= 120000 {
		projection = "column_name, is_identity, is_generated"
	} else if serverVersion >= 100000 {
		projection = "column_name, is_identity, 'NEVER' AS is_generated"
	}
	projection += `,
  pg_catalog.pg_get_serial_sequence(
    pg_catalog.quote_ident(table_schema) || '.' || pg_catalog.quote_ident(table_name),
    column_name
  ) IS NOT NULL AS has_owned_sequence`
	return `
SELECT ` + projection + `
FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = ?
ORDER BY ordinal_position`
}

func postgresOptionMetadataQuery(table string, serverVersion int) string {
	relation := "to_regclass('" + table + "')"
	hasPublication := "FALSE"
	if serverVersion >= 100000 {
		hasPublication = fmt.Sprintf(`EXISTS (
    SELECT 1
    FROM pg_catalog.pg_publication_rel
    WHERE prrelid = %s
  )`, relation)
	}
	hasOIDs := "FALSE"
	if serverVersion < 120000 {
		hasOIDs = "relation.relhasoids"
	}
	hasNonDefaultAccessMethod := "FALSE"
	if serverVersion >= 120000 {
		hasNonDefaultAccessMethod = `relation.relam <> (
        SELECT oid
        FROM pg_catalog.pg_am
        WHERE amname = current_setting('default_table_access_method')
      )`
	}
	return fmt.Sprintf(`
SELECT
  EXISTS (
    SELECT 1
    FROM pg_catalog.pg_class AS relation
    WHERE relation.oid = %[1]s
      AND (
        relation.relkind <> 'r'
        OR relation.relpersistence <> 'p'
        OR relation.reltablespace <> 0
        OR COALESCE(array_length(relation.reloptions, 1), 0) <> 0
        OR relation.relreplident <> 'd'
        OR relation.reloftype <> 0
        OR (%[2]s)
        OR (%[3]s)
      )
  ) OR EXISTS (
    SELECT 1
    FROM pg_catalog.pg_inherits
    WHERE inhrelid = %[1]s
       OR inhparent = %[1]s
  ) OR EXISTS (
    SELECT 1
    FROM pg_catalog.pg_seclabel
    WHERE classoid = 'pg_class'::regclass
      AND objoid = %[1]s
  ) AS has_unsupported_relation_metadata,
  EXISTS (
    SELECT 1 FROM pg_catalog.pg_constraint
    WHERE contype = 'f'
      AND (conrelid = %[1]s OR confrelid = %[1]s)
  ) AS has_foreign_key,
  EXISTS (
    SELECT 1 FROM pg_catalog.pg_trigger
    WHERE tgrelid = %[1]s AND NOT tgisinternal
  ) AS has_trigger,
  EXISTS (
    SELECT 1
    FROM pg_catalog.pg_class
    WHERE oid = %[1]s AND (relrowsecurity OR relforcerowsecurity)
  ) OR EXISTS (
    SELECT 1 FROM pg_catalog.pg_policy
    WHERE polrelid = %[1]s
  ) AS has_rls,
  EXISTS (
    SELECT 1 FROM pg_catalog.pg_rewrite
    WHERE ev_class = %[1]s AND rulename <> '_RETURN'
  ) AS has_rule,
  EXISTS (
    SELECT 1
    FROM pg_catalog.pg_rewrite AS rewrite_meta
    JOIN pg_catalog.pg_class AS dependent
      ON dependent.oid = rewrite_meta.ev_class
    JOIN pg_catalog.pg_depend AS dependency
      ON dependency.classid = 'pg_rewrite'::regclass
     AND dependency.objid = rewrite_meta.oid
    WHERE dependent.relkind IN ('v', 'm')
      AND dependency.refclassid = 'pg_class'::regclass
      AND dependency.refobjid = %[1]s
  ) AS has_dependent_view,
  EXISTS (
    SELECT 1
    FROM pg_catalog.pg_description
    WHERE classoid = 'pg_class'::regclass
      AND objoid = %[1]s
  ) AS has_description,
  EXISTS (
    SELECT 1
    FROM pg_catalog.pg_class
    WHERE oid = %[1]s AND relacl IS NOT NULL
  ) AS has_acl,
  EXISTS (
    SELECT 1
    FROM pg_catalog.pg_attribute
    WHERE attrelid = %[1]s
      AND attnum > 0
      AND NOT attisdropped
      AND attacl IS NOT NULL
  ) AS has_column_acl,
  EXISTS (
    SELECT 1
    FROM pg_catalog.pg_class
    WHERE oid = %[1]s
      AND relowner <> (
        SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user
      )
  ) AS has_other_owner,
  %[4]s AS has_publication`, relation, hasOIDs, hasNonDefaultAccessMethod, hasPublication)
}

func validatePostgresOptionReplacementMetadata(db *gorm.DB) error {
	var serverVersion int
	if err := db.Raw("SHOW server_version_num").Scan(&serverVersion).Error; err != nil {
		return fmt.Errorf("inspect PostgreSQL options replacement metadata: %w", err)
	}
	var features postgresOptionMetadata
	if err := db.Raw(
		postgresOptionMetadataQuery(optionPrimaryKeyTmpTable, serverVersion),
	).Scan(&features).Error; err != nil {
		return fmt.Errorf("inspect PostgreSQL options replacement metadata: %w", err)
	}
	if features.hasUnsupportedSchema() {
		return fmt.Errorf("PostgreSQL options schema cannot be preserved safely")
	}
	return nil
}

func copyPostgresOptionRows(db *gorm.DB, columns []string) error {
	quoted := make([]string, len(columns))
	for i, column := range columns {
		if !optionSafeIdent(column) {
			return fmt.Errorf("PostgreSQL options schema contains an unsafe column identifier")
		}
		quoted[i] = `"` + column + `"`
	}
	columnList := strings.Join(quoted, ", ")
	statement := `INSERT INTO "options_pk_tmp" (` + columnList + `) SELECT ` + columnList + ` FROM "options"`
	if err := db.Exec(statement).Error; err != nil {
		return fmt.Errorf("copy PostgreSQL options rows")
	}
	return nil
}

func validateSQLiteOptionMigrationSchema(db *gorm.DB) (sqliteOptionMigrationSchema, error) {
	var sourceSQL string
	if err := db.Raw(
		"SELECT sql FROM sqlite_master WHERE type = ? AND name = ?",
		"table", "options",
	).Scan(&sourceSQL).Error; err != nil {
		return sqliteOptionMigrationSchema{}, fmt.Errorf("inspect SQLite options table: %w", err)
	}
	hasConflictClause, parsed := sqliteCreateTableHasConflictClause(sourceSQL)
	if hasConflictClause || !parsed {
		return sqliteOptionMigrationSchema{}, fmt.Errorf("SQLite options schema cannot be preserved safely")
	}
	keywords := sqliteSchemaKeywords(sourceSQL)
	if keywords["COLLATE"] {
		return sqliteOptionMigrationSchema{}, fmt.Errorf("SQLite options key collation cannot be preserved safely")
	}
	if keywords["CHECK"] || keywords["STRICT"] || keywords["WITHOUT"] {
		return sqliteOptionMigrationSchema{}, fmt.Errorf("SQLite options schema cannot be preserved safely")
	}
	var columns []sqliteOptionColumn
	if err := db.Raw(`
SELECT name, type, "notnull" AS not_null, dflt_value AS default_value,
       pk AS primary_key, hidden
FROM pragma_table_xinfo(?)
ORDER BY cid`, "options").Scan(&columns).Error; err != nil {
		return sqliteOptionMigrationSchema{}, fmt.Errorf("inspect SQLite options columns: %w", err)
	}
	if len(columns) != 2 ||
		!slices.ContainsFunc(columns, func(column sqliteOptionColumn) bool { return column.Name == "key" }) ||
		!slices.ContainsFunc(columns, func(column sqliteOptionColumn) bool { return column.Name == "value" }) {
		return sqliteOptionMigrationSchema{}, fmt.Errorf("SQLite options schema cannot be preserved safely")
	}
	primaryColumns := 0
	for _, column := range columns {
		if column.PrimaryKey != 0 {
			primaryColumns++
		}
	}
	var schema sqliteOptionMigrationSchema
	for _, column := range columns {
		columnType := strings.ToUpper(strings.ReplaceAll(column.Type, " ", ""))
		typeAllowed := column.Name == "key" && (columnType == "TEXT" || columnType == "VARCHAR(191)") ||
			column.Name == "value" && columnType == "TEXT"
		constraintsAllowed := !column.DefaultValue.Valid && column.NotNull == 0 &&
			column.PrimaryKey == 0 && column.Hidden == 0
		if column.Name == "key" {
			constraintsAllowed = !column.DefaultValue.Valid && column.Hidden == 0 &&
				(column.NotNull == 0 && (column.PrimaryKey == 0 || column.PrimaryKey == 1) ||
					primaryColumns > 1 && column.PrimaryKey != 0 &&
						(column.NotNull == 0 || column.NotNull == 1))
		} else if primaryColumns > 1 && column.PrimaryKey != 0 {
			constraintsAllowed = !column.DefaultValue.Valid &&
				(column.NotNull == 0 || column.NotNull == 1) &&
				column.Hidden == 0
		}
		if !typeAllowed || !constraintsAllowed {
			return sqliteOptionMigrationSchema{}, fmt.Errorf("SQLite options schema cannot be preserved safely")
		}
		if column.Name == "key" {
			schema.KeyType = columnType
		} else {
			schema.ValueNotNull = column.NotNull != 0
		}
	}
	var userTables []string
	if err := db.Raw(`
SELECT name
FROM sqlite_master
WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
ORDER BY name`).Scan(&userTables).Error; err != nil {
		return sqliteOptionMigrationSchema{}, fmt.Errorf("inspect SQLite user tables: %w", err)
	}
	var foreignKeys int64
	for _, table := range userTables {
		var references []struct {
			Table string `gorm:"column:referenced_table"`
		}
		if err := db.Raw(
			`SELECT "table" AS referenced_table FROM pragma_foreign_key_list(?)`,
			table,
		).Scan(&references).Error; err != nil {
			return sqliteOptionMigrationSchema{}, fmt.Errorf("inspect SQLite foreign keys: %w", err)
		}
		if table == "options" {
			foreignKeys += int64(len(references))
			continue
		}
		for _, reference := range references {
			if strings.EqualFold(reference.Table, "options") {
				return sqliteOptionMigrationSchema{}, fmt.Errorf("SQLite options schema cannot be preserved safely")
			}
		}
	}
	var triggers []struct {
		Name string         `gorm:"column:name"`
		SQL  sql.NullString `gorm:"column:sql"`
	}
	if err := db.Raw(`
SELECT name, sql
FROM sqlite_master
WHERE type = 'trigger' AND name NOT LIKE 'sqlite_%'
ORDER BY name`).Scan(&triggers).Error; err != nil {
		return sqliteOptionMigrationSchema{}, fmt.Errorf("inspect SQLite options triggers: %w", err)
	}
	if foreignKeys != 0 {
		return sqliteOptionMigrationSchema{}, fmt.Errorf("SQLite options schema cannot be preserved safely")
	}
	for _, trigger := range triggers {
		if !trigger.SQL.Valid {
			return sqliteOptionMigrationSchema{}, fmt.Errorf("SQLite options schema cannot be preserved safely")
		}
		referencesOptions, parsed := sqliteTriggerReferencesTable(trigger.SQL.String, "options")
		if referencesOptions || !parsed {
			return sqliteOptionMigrationSchema{}, fmt.Errorf("SQLite options schema cannot be preserved safely")
		}
	}
	var views []struct {
		SQL string `gorm:"column:sql"`
	}
	if err := db.Raw(`
SELECT sql
FROM sqlite_master
WHERE type = 'view' AND sql IS NOT NULL
ORDER BY name`).Scan(&views).Error; err != nil {
		return sqliteOptionMigrationSchema{}, fmt.Errorf("inspect SQLite views: %w", err)
	}
	for _, view := range views {
		referencesOptions, parsed := sqliteViewReferencesTable(view.SQL, "options")
		if referencesOptions || !parsed {
			return sqliteOptionMigrationSchema{}, fmt.Errorf("SQLite options schema cannot be preserved safely")
		}
	}
	var indexes []sqliteOptionIndex
	if err := db.Raw(
		"SELECT name, `unique`, origin, partial FROM pragma_index_list(?) ORDER BY seq",
		"options",
	).Scan(&indexes).Error; err != nil {
		return sqliteOptionMigrationSchema{}, fmt.Errorf("inspect SQLite options indexes: %w", err)
	}
	for _, index := range indexes {
		if index.Unique != 1 || index.Partial != 0 {
			return sqliteOptionMigrationSchema{}, fmt.Errorf("SQLite options schema cannot be preserved safely")
		}
		var indexedColumns []sqliteOptionIndexColumn
		if err := db.Raw(
			"SELECT name, `desc` AS descending, coll AS collation "+
				"FROM pragma_index_xinfo(?) WHERE `key` = 1 ORDER BY seqno",
			index.Name,
		).Scan(&indexedColumns).Error; err != nil {
			return sqliteOptionMigrationSchema{}, fmt.Errorf("inspect SQLite options index: %w", err)
		}
		if index.Origin == "pk" && primaryColumns > 1 &&
			len(indexedColumns) == primaryColumns &&
			!slices.ContainsFunc(indexedColumns, func(column sqliteOptionIndexColumn) bool {
				return !column.Name.Valid ||
					column.Name.String != "key" && column.Name.String != "value" ||
					column.Descending != 0 ||
					!strings.EqualFold(column.Collation, "BINARY")
			}) {
			continue
		}
		if len(indexedColumns) != 1 ||
			!indexedColumns[0].Name.Valid ||
			indexedColumns[0].Name.String != "key" ||
			indexedColumns[0].Descending != 0 ||
			!strings.EqualFold(indexedColumns[0].Collation, "BINARY") {
			return sqliteOptionMigrationSchema{}, fmt.Errorf("SQLite options schema cannot be preserved safely")
		}
	}
	return schema, nil
}

func sqliteCreateTableHasConflictClause(statement string) (bool, bool) {
	tokens, complete := sqliteLexSQL(statement)
	if !complete || len(tokens) == 0 || !sqliteUnquotedToken(tokens[0], "CREATE") {
		return false, false
	}
	position := 1
	if position < len(tokens) &&
		(sqliteUnquotedToken(tokens[position], "TEMP") ||
			sqliteUnquotedToken(tokens[position], "TEMPORARY")) {
		position++
	}
	if position >= len(tokens) || !sqliteUnquotedToken(tokens[position], "TABLE") {
		return false, false
	}
	position++
	if position < len(tokens) && sqliteUnquotedToken(tokens[position], "IF") {
		if position+2 >= len(tokens) ||
			!sqliteUnquotedToken(tokens[position+1], "NOT") ||
			!sqliteUnquotedToken(tokens[position+2], "EXISTS") {
			return false, false
		}
		position += 3
	}
	_, _, position, complete = sqliteRelationName(tokens, position)
	if !complete || position >= len(tokens) || !sqlitePunctuationToken(tokens[position], "(") {
		return false, false
	}
	close, complete := sqliteMatchingParen(tokens, position)
	if !complete || close == position+1 {
		return false, false
	}

	definitionStart := position + 1
	depth := 0
	for current := definitionStart; current <= close; current++ {
		if current < close && tokens[current].Kind == sqliteSQLTokenPunctuation {
			switch tokens[current].Text {
			case "(":
				depth++
			case ")":
				depth--
				if depth < 0 {
					return false, false
				}
			}
		}
		if current < close && (!sqlitePunctuationToken(tokens[current], ",") || depth != 0) {
			continue
		}
		if current == definitionStart {
			return false, false
		}
		hasConflict, parsed := sqliteTableDefinitionHasConflictClause(
			tokens[definitionStart:current],
		)
		if hasConflict || !parsed {
			return hasConflict, parsed
		}
		definitionStart = current + 1
	}
	return false, depth == 0 && definitionStart == close+1
}

func sqliteTableDefinitionHasConflictClause(tokens []sqliteSQLToken) (bool, bool) {
	conflictCapableConstraint := false
	depth := 0
	for position := 0; position < len(tokens); position++ {
		token := tokens[position]
		if token.Kind == sqliteSQLTokenPunctuation {
			switch token.Text {
			case "(":
				depth++
			case ")":
				depth--
				if depth < 0 {
					return false, false
				}
			}
			continue
		}
		if depth != 0 {
			continue
		}
		switch {
		case sqliteUnquotedToken(token, "NOT"):
			if position+1 < len(tokens) && sqliteUnquotedToken(tokens[position+1], "NULL") {
				conflictCapableConstraint = true
			}
		case sqliteUnquotedToken(token, "PRIMARY"):
			if position+1 < len(tokens) && sqliteUnquotedToken(tokens[position+1], "KEY") {
				conflictCapableConstraint = true
			}
		case sqliteUnquotedToken(token, "UNIQUE"):
			conflictCapableConstraint = true
		case sqliteUnquotedToken(token, "ON"):
			if position+1 >= len(tokens) ||
				!sqliteUnquotedToken(tokens[position+1], "CONFLICT") {
				continue
			}
			if !conflictCapableConstraint || position+2 >= len(tokens) ||
				!sqliteUnquotedToken(tokens[position+2], "") ||
				!sqliteConflictActions[tokens[position+2].Text] {
				return false, false
			}
			return true, true
		}
	}
	return false, depth == 0
}

func sqliteUnquotedToken(token sqliteSQLToken, text string) bool {
	if token.Kind != sqliteSQLTokenKeyword && token.Kind != sqliteSQLTokenIdentifier {
		return false
	}
	return text == "" || token.Text == text
}

func sqliteSchemaKeywords(statement string) map[string]bool {
	tokens := make(map[string]bool)
	sqlTokens, _ := sqliteLexSQL(statement)
	for _, token := range sqlTokens {
		if token.Kind == sqliteSQLTokenKeyword || token.Kind == sqliteSQLTokenIdentifier {
			tokens[token.Text] = true
		}
	}
	return tokens
}

type sqliteSQLTokenKind uint8

const (
	sqliteSQLTokenKeyword sqliteSQLTokenKind = iota
	sqliteSQLTokenIdentifier
	sqliteSQLTokenQuotedIdentifier
	sqliteSQLTokenStringLiteral
	sqliteSQLTokenPunctuation
)

type sqliteSQLToken struct {
	Kind sqliteSQLTokenKind
	Text string
}

var sqliteRelationKeywords = map[string]bool{
	"ABORT": true, "ALL": true, "AS": true, "BEGIN": true, "BY": true,
	"CREATE": true, "CROSS": true, "DELETE": true, "DISTINCT": true,
	"END": true, "EXCEPT": true, "FAIL": true, "FROM": true, "FULL": true,
	"GROUP": true, "HAVING": true, "IGNORE": true, "INNER": true,
	"INSERT": true, "INTERSECT": true, "INTO": true, "JOIN": true,
	"LEFT": true, "LIMIT": true, "NATURAL": true, "OFFSET": true,
	"ON": true, "OR": true, "ORDER": true, "OUTER": true,
	"RECURSIVE": true, "REPLACE": true, "RETURNING": true,
	"RIGHT": true, "ROLLBACK": true, "SELECT": true, "TEMP": true,
	"TEMPORARY": true, "TRIGGER": true, "UNION": true, "UPDATE": true,
	"USING": true, "VALUES": true, "VIEW": true, "WHERE": true,
	"WINDOW": true, "WITH": true,
}

func sqliteLexSQL(statement string) ([]sqliteSQLToken, bool) {
	tokens := make([]sqliteSQLToken, 0, len(statement)/4)
	for i := 0; i < len(statement); {
		switch statement[i] {
		case '\'':
			quote := statement[i]
			i++
			var literal strings.Builder
			for i < len(statement) {
				if statement[i] != quote {
					literal.WriteByte(statement[i])
					i++
					continue
				}
				i++
				if i < len(statement) && statement[i] == quote {
					literal.WriteByte(quote)
					i++
					continue
				}
				tokens = append(tokens, sqliteSQLToken{
					Kind: sqliteSQLTokenStringLiteral,
					Text: literal.String(),
				})
				goto nextToken
			}
			return tokens, false
		case '"', '`':
			quote := statement[i]
			i++
			var identifier strings.Builder
			for i < len(statement) {
				if statement[i] != quote {
					identifier.WriteByte(statement[i])
					i++
					continue
				}
				i++
				if i < len(statement) && statement[i] == quote {
					identifier.WriteByte(quote)
					i++
					continue
				}
				tokens = append(tokens, sqliteSQLToken{
					Kind: sqliteSQLTokenQuotedIdentifier,
					Text: strings.ToUpper(identifier.String()),
				})
				goto nextToken
			}
			return tokens, false
		case '[':
			i++
			start := i
			for i < len(statement) && statement[i] != ']' {
				i++
			}
			if i == len(statement) {
				return tokens, false
			}
			tokens = append(tokens, sqliteSQLToken{
				Kind: sqliteSQLTokenQuotedIdentifier,
				Text: strings.ToUpper(statement[start:i]),
			})
			i++
		case '-':
			if i+1 < len(statement) && statement[i+1] == '-' {
				i += 2
				for i < len(statement) && statement[i] != '\n' {
					i++
				}
				continue
			}
			tokens = append(tokens, sqliteSQLToken{
				Kind: sqliteSQLTokenPunctuation,
				Text: string(statement[i]),
			})
			i++
		case '/':
			if i+1 < len(statement) && statement[i+1] == '*' {
				i += 2
				for i+1 < len(statement) && (statement[i] != '*' || statement[i+1] != '/') {
					i++
				}
				if i+1 >= len(statement) {
					return tokens, false
				}
				i += 2
				continue
			}
			tokens = append(tokens, sqliteSQLToken{
				Kind: sqliteSQLTokenPunctuation,
				Text: string(statement[i]),
			})
			i++
		default:
			if statement[i] == ' ' || statement[i] == '\t' ||
				statement[i] == '\r' || statement[i] == '\n' {
				i++
				continue
			}
			if !sqliteIdentByte(statement[i]) {
				tokens = append(tokens, sqliteSQLToken{
					Kind: sqliteSQLTokenPunctuation,
					Text: string(statement[i]),
				})
				i++
				continue
			}
			start := i
			for i < len(statement) && sqliteIdentByte(statement[i]) {
				i++
			}
			text := strings.ToUpper(statement[start:i])
			kind := sqliteSQLTokenIdentifier
			if sqliteRelationKeywords[text] {
				kind = sqliteSQLTokenKeyword
			}
			tokens = append(tokens, sqliteSQLToken{Kind: kind, Text: text})
		}
	nextToken:
	}
	return tokens, true
}

// sqliteViewReferencesTable recognizes relation positions used by SQLite views;
// it deliberately does not attempt to parse the complete SQL grammar.
func sqliteViewReferencesTable(statement, table string) (bool, bool) {
	tokens, complete := sqliteLexSQL(statement)
	if !complete || len(tokens) == 0 {
		return false, false
	}
	queryStart, ok := sqliteViewQueryStart(tokens)
	if !ok {
		return false, false
	}
	return sqliteQueryReferencesTable(tokens[queryStart:], strings.ToUpper(table), nil)
}

func sqliteTriggerReferencesTable(statement, table string) (bool, bool) {
	tokens, complete := sqliteLexSQL(statement)
	if !complete || len(tokens) == 0 ||
		tokens[0].Kind != sqliteSQLTokenKeyword || tokens[0].Text != "CREATE" {
		return false, false
	}
	target := strings.ToUpper(table)
	sawTrigger := false
	ownerStart := -1
	begin := -1
	depth := 0
	for i, token := range tokens {
		if token.Kind == sqliteSQLTokenPunctuation {
			switch token.Text {
			case "(":
				depth++
			case ")":
				depth--
				if depth < 0 {
					return false, false
				}
			}
			continue
		}
		if depth != 0 || token.Kind != sqliteSQLTokenKeyword {
			continue
		}
		switch token.Text {
		case "TRIGGER":
			if sawTrigger {
				return false, false
			}
			sawTrigger = true
		case "ON":
			if sawTrigger && ownerStart < 0 {
				ownerStart = i + 1
			}
		case "BEGIN":
			if sawTrigger {
				begin = i
			}
		}
		if begin >= 0 {
			break
		}
	}
	if !sawTrigger || ownerStart < 0 || begin < 0 || ownerStart >= begin {
		return false, false
	}
	owner, _, ownerEnd, ok := sqliteRelationName(tokens, ownerStart)
	if !ok || ownerEnd > begin {
		return false, false
	}
	if owner == target {
		return true, true
	}
	if ownerEnd < begin {
		references, parsed := sqliteQueryReferencesTable(tokens[ownerEnd:begin], target, nil)
		if references || !parsed {
			return references, parsed
		}
	}

	end := len(tokens) - 1
	if sqlitePunctuationToken(tokens[end], ";") {
		end--
	}
	if end <= begin || tokens[end].Kind != sqliteSQLTokenKeyword || tokens[end].Text != "END" {
		return false, false
	}
	statementStart := begin + 1
	sawStatement := false
	depth = 0
	for position := statementStart; position <= end; position++ {
		if position < end && tokens[position].Kind == sqliteSQLTokenPunctuation {
			switch tokens[position].Text {
			case "(":
				depth++
			case ")":
				depth--
				if depth < 0 {
					return false, false
				}
			}
		}
		if position < end && (!sqlitePunctuationToken(tokens[position], ";") || depth != 0) {
			continue
		}
		if position == statementStart {
			if position == end && sawStatement {
				break
			}
			return false, false
		}
		references, parsed := sqliteQueryReferencesTable(
			tokens[statementStart:position], target, nil,
		)
		if references || !parsed {
			return references, parsed
		}
		sawStatement = true
		statementStart = position + 1
	}
	return false, sawStatement && depth == 0
}

func sqliteViewQueryStart(tokens []sqliteSQLToken) (int, bool) {
	if tokens[0].Text != "CREATE" {
		return 0, true
	}
	depth := 0
	sawView := false
	for i, token := range tokens {
		if token.Kind == sqliteSQLTokenPunctuation {
			switch token.Text {
			case "(":
				depth++
			case ")":
				depth--
				if depth < 0 {
					return 0, false
				}
			}
			continue
		}
		if depth != 0 || token.Kind != sqliteSQLTokenKeyword {
			continue
		}
		if token.Text == "VIEW" {
			sawView = true
			continue
		}
		if sawView && token.Text == "AS" {
			if i+1 == len(tokens) {
				return 0, false
			}
			return i + 1, true
		}
	}
	return 0, false
}

func sqliteQueryReferencesTable(
	tokens []sqliteSQLToken,
	target string,
	inheritedCTEs map[string]bool,
) (bool, bool) {
	if len(tokens) == 0 {
		return false, false
	}
	ctes := maps.Clone(inheritedCTEs)
	if ctes == nil {
		ctes = make(map[string]bool)
	}
	position := 0
	if tokens[position].Kind == sqliteSQLTokenKeyword && tokens[position].Text == "WITH" {
		position++
		if position < len(tokens) && tokens[position].Text == "RECURSIVE" {
			position++
		}
		type cteBodyRange struct {
			start int
			end   int
		}
		var bodies []cteBodyRange
		declared := make(map[string]bool)
		for {
			if position >= len(tokens) || !sqliteIdentifierToken(tokens[position]) {
				return false, false
			}
			cteName := tokens[position].Text
			if declared[cteName] {
				return false, false
			}
			declared[cteName] = true
			ctes[cteName] = true
			position++
			if position < len(tokens) && sqlitePunctuationToken(tokens[position], "(") {
				close, ok := sqliteMatchingParen(tokens, position)
				if !ok || !sqliteCTEColumnListValid(tokens[position+1:close]) {
					return false, false
				}
				position = close + 1
			}
			if position >= len(tokens) || tokens[position].Text != "AS" {
				return false, false
			}
			position++
			if position < len(tokens) && tokens[position].Text == "NOT" {
				position++
				if position >= len(tokens) || tokens[position].Text != "MATERIALIZED" {
					return false, false
				}
				position++
			} else if position < len(tokens) && tokens[position].Text == "MATERIALIZED" {
				position++
			}
			if position >= len(tokens) || !sqlitePunctuationToken(tokens[position], "(") {
				return false, false
			}
			close, ok := sqliteMatchingParen(tokens, position)
			if !ok || close == position+1 {
				return false, false
			}
			bodies = append(bodies, cteBodyRange{start: position + 1, end: close})
			position = close + 1
			if position < len(tokens) && sqlitePunctuationToken(tokens[position], ",") {
				position++
				continue
			}
			break
		}
		if position == len(tokens) {
			return false, false
		}
		for _, body := range bodies {
			references, parsed := sqliteQueryReferencesTable(
				tokens[body.start:body.end], target, ctes,
			)
			if references || !parsed {
				return references, parsed
			}
		}
	}

	inFrom := false
	expectRelation := false
	expectUpdateRelation := false
	for position < len(tokens) {
		token := tokens[position]
		if token.Kind == sqliteSQLTokenPunctuation {
			switch token.Text {
			case "(":
				close, ok := sqliteMatchingParen(tokens, position)
				if !ok || expectRelation && close == position+1 {
					return false, false
				}
				expectRelation = false
				expectUpdateRelation = false
				if close == position+1 {
					position = close + 1
					continue
				}
				references, parsed := sqliteQueryReferencesTable(
					tokens[position+1:close], target, ctes,
				)
				if references || !parsed {
					return references, parsed
				}
				position = close + 1
				continue
			case ")":
				return false, false
			case ",":
				if inFrom {
					expectRelation = true
					expectUpdateRelation = false
				}
			case ";":
				if position != len(tokens)-1 {
					return false, false
				}
			}
			position++
			continue
		}

		if expectRelation {
			if expectUpdateRelation && token.Kind == sqliteSQLTokenKeyword && token.Text == "OR" {
				position++
				if position >= len(tokens) ||
					tokens[position].Kind != sqliteSQLTokenKeyword ||
					!sqliteConflictActions[tokens[position].Text] {
					return false, false
				}
				position++
				if position >= len(tokens) {
					return false, false
				}
			}
			relation, qualified, next, ok := sqliteRelationName(tokens, position)
			if !ok {
				return false, false
			}
			if relation == target && (qualified || !ctes[relation]) {
				return true, true
			}
			expectRelation = false
			expectUpdateRelation = false
			position = next
			continue
		}

		if token.Kind == sqliteSQLTokenKeyword {
			switch token.Text {
			case "FROM":
				inFrom = true
				expectRelation = true
				expectUpdateRelation = false
			case "JOIN":
				inFrom = true
				expectRelation = true
				expectUpdateRelation = false
			case "UPDATE":
				expectRelation = true
				expectUpdateRelation = true
			case "INTO":
				expectRelation = true
				expectUpdateRelation = false
			case "WHERE", "GROUP", "HAVING", "WINDOW", "ORDER", "LIMIT",
				"RETURNING", "UNION", "INTERSECT", "EXCEPT":
				inFrom = false
				expectRelation = false
				expectUpdateRelation = false
			case "SELECT", "VALUES":
				expectRelation = false
				expectUpdateRelation = false
			case "WITH":
				return false, false
			}
		}
		position++
	}
	if expectRelation {
		return false, false
	}
	return false, true
}

var sqliteConflictActions = map[string]bool{
	"ROLLBACK": true,
	"ABORT":    true,
	"REPLACE":  true,
	"FAIL":     true,
	"IGNORE":   true,
}

func sqliteRelationName(tokens []sqliteSQLToken, position int) (string, bool, int, bool) {
	if position >= len(tokens) || !sqliteIdentifierToken(tokens[position]) {
		return "", false, position, false
	}
	relation := tokens[position].Text
	qualified := false
	position++
	for position < len(tokens) && sqlitePunctuationToken(tokens[position], ".") {
		if position+1 >= len(tokens) || !sqliteIdentifierToken(tokens[position+1]) {
			return "", false, position, false
		}
		qualified = true
		relation = tokens[position+1].Text
		position += 2
	}
	return relation, qualified, position, true
}

func sqliteIdentifierToken(token sqliteSQLToken) bool {
	return token.Kind == sqliteSQLTokenIdentifier ||
		token.Kind == sqliteSQLTokenQuotedIdentifier
}

func sqlitePunctuationToken(token sqliteSQLToken, punctuation string) bool {
	return token.Kind == sqliteSQLTokenPunctuation && token.Text == punctuation
}

func sqliteMatchingParen(tokens []sqliteSQLToken, open int) (int, bool) {
	if open >= len(tokens) || !sqlitePunctuationToken(tokens[open], "(") {
		return 0, false
	}
	depth := 0
	for i := open; i < len(tokens); i++ {
		if tokens[i].Kind != sqliteSQLTokenPunctuation {
			continue
		}
		switch tokens[i].Text {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return i, true
			}
			if depth < 0 {
				return 0, false
			}
		}
	}
	return 0, false
}

func sqliteCTEColumnListValid(tokens []sqliteSQLToken) bool {
	if len(tokens) == 0 {
		return false
	}
	expectIdentifier := true
	for _, token := range tokens {
		if expectIdentifier {
			if !sqliteIdentifierToken(token) {
				return false
			}
		} else if !sqlitePunctuationToken(token, ",") {
			return false
		}
		expectIdentifier = !expectIdentifier
	}
	return !expectIdentifier
}

func sqliteIdentByte(value byte) bool {
	return value == '_' || value >= '0' && value <= '9' ||
		value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' ||
		value >= 0x80
}

func createTransactionalOptionReplacement(db *gorm.DB, sqliteSchema sqliteOptionMigrationSchema) error {
	switch db.Dialector.Name() {
	case "postgres":
		if err := db.Exec("CREATE TABLE options_pk_tmp (LIKE options INCLUDING ALL)").Error; err != nil {
			return err
		}
		if err := db.Exec("ALTER TABLE options_pk_tmp ALTER COLUMN key SET NOT NULL").Error; err != nil {
			return err
		}
		return db.Exec("ALTER TABLE options_pk_tmp ADD PRIMARY KEY (key)").Error
	case "sqlite":
		if sqliteSchema.KeyType != "TEXT" && sqliteSchema.KeyType != "VARCHAR(191)" {
			return fmt.Errorf("unsupported SQLite options key type")
		}
		valueDefinition := "TEXT"
		if sqliteSchema.ValueNotNull {
			valueDefinition += " NOT NULL"
		}
		return db.Exec(
			"CREATE TABLE `options_pk_tmp` (`key` " + sqliteSchema.KeyType +
				" NOT NULL PRIMARY KEY, `value` " + valueDefinition + ")",
		).Error
	default:
		return db.Table(optionPrimaryKeyTmpTable).Migrator().CreateTable(&Option{})
	}
}

func repairMySQLOptionPrimaryKey(db *gorm.DB, digestKey []byte, artifacts mysqlOptionMigrationArtifacts) error {
	columns, columnErr := mysqlWritableOptionColumns(db)
	if columnErr != nil {
		return columnErr
	}
	if err := validateMySQLOptionMigrationSchema(db); err != nil {
		return err
	}
	if err := validateMySQLOptionDeduplicationColumns(db, columns); err != nil {
		return err
	}
	if _, _, err := readDedupedOptionRows(db, digestKey); err != nil {
		return err
	}
	sourceDDL, err := showCreateMySQLTable(db, "options")
	if err != nil {
		return err
	}
	sourceDigest := sha256.Sum256([]byte(sourceDDL))
	artifacts.SourceSchemaSHA256 = hex.EncodeToString(sourceDigest[:])
	if err := createOwnedMySQLOptionMigrationTable(db, sourceDDL, artifacts); err != nil {
		return fmt.Errorf("create %s from source schema: %w", artifacts.TmpTable, err)
	}
	if err := db.Exec(
		"ALTER TABLE " + quoteMySQLIdent(artifacts.TmpTable) + " ADD PRIMARY KEY (`key`)",
	).Error; err != nil {
		return fmt.Errorf("add options replacement primary key: %w", err)
	}
	if err := recordMySQLOptionActiveSchema(db, &artifacts); err != nil {
		return err
	}
	if err := createMySQLOptionMigrationTriggers(db, columns, artifacts); err != nil {
		return err
	}
	if err := db.Exec(
		"LOCK TABLES `options` WRITE, " + quoteMySQLIdent(artifacts.TmpTable) +
			" WRITE, `options` AS source_row READ",
	).Error; err != nil {
		return fmt.Errorf("lock options replacement tables: %w", err)
	}

	if err := validateLockedMySQLOptionMigrationSchema(db, sourceDDL, artifacts); err != nil {
		return err
	}
	if err := validateMySQLOptionDeduplicationColumns(db, columns); err != nil {
		return err
	}
	rows, deduped, err := readDedupedOptionRows(db, digestKey)
	if err != nil {
		return err
	}
	if err := insertMySQLDedupedOptionRows(db, columns, deduped, artifacts.TmpTable); err != nil {
		return err
	}
	var written int64
	if err := db.Table(artifacts.TmpTable).Count(&written).Error; err != nil {
		return fmt.Errorf("count rebuilt options: %w", err)
	}
	if int(written) != len(deduped) {
		return fmt.Errorf("options rebuild wrote %d rows, want %d", written, len(deduped))
	}
	if err := db.Exec("UNLOCK TABLES").Error; err != nil {
		return fmt.Errorf("unlock options replacement tables: %w", err)
	}

	if err := swapOptionTables(db, artifacts.TmpTable, artifacts.BackupTable); err != nil {
		return err
	}
	if err := validateRenamedMySQLOptionMigrationSchema(db, sourceDDL, artifacts.BackupTable, artifacts); err != nil {
		return err
	}
	if err := finalizeMySQLOptionMigration(db, artifacts, optionMigrationPrepared); err != nil {
		return err
	}
	primary, err := optionsKeyIsPrimary(db)
	if err != nil {
		return err
	}
	if !primary {
		return fmt.Errorf("options table still has no primary key after rebuild")
	}
	common.SysLog(fmt.Sprintf(
		"rebuilt options table with primary key from %d rows into %d keys; previous rows kept in %s",
		len(rows), len(deduped), artifacts.BackupTable,
	))
	return nil
}

func validateLockedMySQLOptionMigrationSchema(
	db *gorm.DB,
	sourceDDL string,
	artifacts mysqlOptionMigrationArtifacts,
) error {
	currentDDL, err := showCreateMySQLTable(db, "options")
	if err != nil {
		return fmt.Errorf("inspect locked MySQL options schema: %w", err)
	}
	if currentDDL != sourceDDL {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	if err := validateMySQLOptionMigrationTriggerSet(db, "options", artifacts); err != nil {
		return err
	}
	return validateMySQLOptionIncomingForeignKeys(db, "options")
}

func validateRenamedMySQLOptionMigrationSchema(
	db *gorm.DB,
	sourceDDL string,
	backup string,
	artifacts mysqlOptionMigrationArtifacts,
) error {
	expectedDDL, ok := replaceMySQLCreateTableName(sourceDDL, "options", backup)
	if !ok {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	currentDDL, err := showCreateMySQLTable(db, backup)
	if err != nil || currentDDL != expectedDDL {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	if err := validateMySQLOptionMigrationTriggerSet(db, backup, artifacts); err != nil {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	state, hasState, err := inspectMySQLOptionMigrationState(db, "options")
	if err != nil || !hasState {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	if err := validateMySQLOptionActiveSchema(db, "options", artifacts, state); err != nil {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	return validateMySQLOptionIncomingForeignKeys(db, "options", backup)
}

func validateMySQLOptionMigrationTriggerSet(
	db *gorm.DB,
	table string,
	artifacts mysqlOptionMigrationArtifacts,
) error {
	var triggerCount int64
	if err := db.Raw(`
SELECT count(*)
FROM information_schema.triggers
WHERE trigger_schema = DATABASE() AND event_object_table = ?`, table).
		Scan(&triggerCount).Error; err != nil {
		return fmt.Errorf("inspect MySQL options schema metadata: %w", err)
	}
	triggers, err := inspectOwnedMySQLOptionMigrationTriggers(db, artifacts)
	if err != nil {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	if triggerCount != 3 || len(triggers) != 3 ||
		slices.ContainsFunc(triggers, func(trigger mysqlOptionTrigger) bool {
			return trigger.TableName != table
		}) {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	return nil
}

func recoverActiveMySQLOptionMigration(db *gorm.DB) (
	recovered bool,
	resultErr error,
) {
	var activeMarkers []struct {
		Marker string `gorm:"column:column_comment"`
	}
	if err := db.Raw(`
SELECT column_comment
FROM information_schema.columns
WHERE table_schema = DATABASE()
  AND table_name = 'options'
  AND column_name = ?
  AND data_type = 'tinyint'
  AND column_type LIKE '%unsigned'
  AND is_nullable = 'NO'
  AND column_default = '1'`, optionMigrationMarkerCol).Scan(&activeMarkers).Error; err != nil {
		return false, fmt.Errorf("inspect MySQL migration markers: %w", err)
	}
	if len(activeMarkers) == 0 {
		var stateColumns int64
		if err := db.Raw(`
SELECT count(*)
FROM information_schema.columns
WHERE table_schema = DATABASE() AND table_name = 'options' AND column_name = ?`,
			optionMigrationStateCol).Scan(&stateColumns).Error; err != nil {
			return false, fmt.Errorf("inspect MySQL migration state: %w", err)
		}
		if stateColumns != 0 {
			return false, fmt.Errorf("options migration artifact collision: active state without owner")
		}
		return false, nil
	}
	if len(activeMarkers) != 1 {
		return false, fmt.Errorf("options migration artifact collision: active options ownership marker")
	}
	artifacts, ok := parseMySQLOptionMigrationArtifacts("options", activeMarkers[0].Marker)
	if !ok {
		return false, fmt.Errorf("options migration artifact collision: marker table=options")
	}

	state, hasState, err := inspectMySQLOptionMigrationState(db, "options")
	if err != nil {
		return false, err
	}
	if !hasState {
		return false, fmt.Errorf("options migration recovery incomplete: legacy active replacement")
	}

	artifacts.BackupTable = optionLegacyTablePrefix + artifacts.ID
	artifacts.SourceSchemaSHA256 = state.SourceSchemaSHA256
	artifacts.ActiveSchemaSHA256 = state.ActiveSchemaSHA256
	if err := validateMySQLGlobalTableMetadataVisibility(db); err != nil {
		return false, err
	}
	journal, hasJournal, err := inspectMySQLOptionTerminalJournal(db)
	if err != nil {
		return false, err
	}
	if hasJournal {
		defer func() {
			if resultErr != nil {
				resultErr = mysqlOptionTerminalJournalRecoveryBlocked(resultErr)
			}
		}()
		if err := validateMySQLOptionTerminalJournalSnapshot(
			db, artifacts, journal,
		); err != nil {
			return false, err
		}
	}
	restoreIntent, hasRestoreIntent := parseMySQLOptionRestoreIntent(state.Phase)
	forwardIntent, hasForwardIntent := parseMySQLOptionForwardIntent(state.Phase)
	targetPhase := state.Phase
	if hasRestoreIntent {
		targetPhase = restoreIntent.TargetPhase
	} else if hasForwardIntent {
		targetPhase = forwardIntent.TargetPhase
	}
	bridgePhase, _, err := inspectMySQLOptionFinalizationBridgePhase(
		db, artifacts, state, targetPhase,
	)
	if err != nil {
		return false, err
	}
	if hasRestoreIntent {
		if hasJournal {
			return false, fmt.Errorf("options migration artifact collision: completion journal")
		}
		targetState := state
		targetState.Phase = targetPhase
		if err := resumeMySQLOptionFinalizationRestore(
			db, artifacts, targetState, restoreIntent,
		); err != nil {
			return false, err
		}
		state = targetState
	} else if hasForwardIntent {
		targetState := state
		targetState.Phase = targetPhase
		var expectedJournal *mysqlOptionTerminalJournal
		if hasJournal {
			expectedJournal = &journal
		}
		if err := resumeMySQLOptionFinalizationForward(
			db, artifacts, targetState, forwardIntent, expectedJournal,
		); err != nil {
			return false, err
		}
		state = targetState
	} else if bridgePhase != state.Phase {
		return false, fmt.Errorf("options migration recovery incomplete: invalid state/bridge phase")
	}
	if err := validateMySQLOptionFinalizationState(db, artifacts, state); err != nil {
		return false, err
	}
	var existingJournal *mysqlOptionTerminalJournal
	if hasJournal {
		existingJournal = &journal
	}
	if err := finalizeMySQLOptionMigrationWithJournal(
		db, artifacts, state.Phase, existingJournal,
	); err != nil {
		return false, err
	}
	return true, nil
}

func inspectMySQLOptionFinalizationBridgePhase(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	state mysqlOptionMigrationState,
	targetPhase string,
) (string, []mysqlOptionTrigger, error) {
	current, err := inspectMySQLOptionMigrationTriggerCandidates(db, artifacts)
	if err != nil {
		return "", nil, err
	}
	currentNames := make([]string, len(current))
	for i, trigger := range current {
		currentNames[i] = trigger.Name
	}
	slices.Sort(currentNames)

	bridgePhase := ""
	for _, phase := range []string{
		optionMigrationPrepared,
		optionMigrationInsertGone,
		optionMigrationUpdateGone,
		optionMigrationBridgesGone,
	} {
		expectedNames, _ := mysqlOptionMigrationBridgeNamesForPhase(artifacts, phase)
		slices.Sort(expectedNames)
		if slices.Equal(currentNames, expectedNames) {
			bridgePhase = phase
			break
		}
	}
	if bridgePhase == "" {
		return "", nil, fmt.Errorf("MySQL options schema changed during migration")
	}
	if bridgePhase == optionMigrationPrepared && targetPhase == optionMigrationValidated {
		bridgePhase = optionMigrationValidated
	}
	if err := validateMySQLOptionFinalizationStateForBridgePhase(
		db, artifacts, state, bridgePhase,
	); err != nil {
		return "", nil, err
	}

	columns, err := mysqlWritableOptionColumnsFromTable(db, artifacts.BackupTable)
	if err != nil {
		return "", nil, err
	}
	specs := mysqlOptionMigrationTriggerSpecs(columns, artifacts)
	targetNames, validTarget := mysqlOptionMigrationBridgeNamesForPhase(artifacts, targetPhase)
	if !validTarget {
		return "", nil, fmt.Errorf("options migration recovery incomplete: invalid target phase")
	}
	targetSet := make(map[string]struct{}, len(targetNames))
	for _, name := range targetNames {
		targetSet[name] = struct{}{}
	}
	retainedTriggers := make([]mysqlOptionTrigger, 0, len(targetNames))
	for _, spec := range specs {
		if _, needed := targetSet[spec.Name]; !needed {
			continue
		}
		retainedTriggers = append(retainedTriggers, mysqlOptionTrigger{
			Name:      spec.Name,
			Event:     spec.Event,
			Timing:    "AFTER",
			TableName: artifacts.BackupTable,
			Action:    spec.Action,
		})
	}
	return bridgePhase, retainedTriggers, nil
}

func validateMySQLOptionFinalizationState(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	state mysqlOptionMigrationState,
) error {
	return validateMySQLOptionFinalizationStateForBridgePhase(
		db, artifacts, state, state.Phase,
	)
}

func validateMySQLOptionFinalizationStateForBridgePhase(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	state mysqlOptionMigrationState,
	bridgePhase string,
) error {
	if err := validateMySQLGlobalTableMetadataVisibility(db); err != nil {
		return err
	}
	backupDDL, err := showCreateMySQLTable(db, artifacts.BackupTable)
	if err != nil {
		return fmt.Errorf("inspect retained MySQL options schema: %w", err)
	}
	normalizedDDL, ok := replaceMySQLCreateTableName(backupDDL, artifacts.BackupTable, "options")
	if !ok {
		return fmt.Errorf("options migration recovery incomplete: retained source schema")
	}
	actualDigest := sha256.Sum256([]byte(normalizedDDL))
	schemaMatches := hmac.Equal(
		[]byte(state.SourceSchemaSHA256),
		[]byte(hex.EncodeToString(actualDigest[:])),
	)
	activeSchemaMatches := validateMySQLOptionActiveSchema(db, "options", artifacts, state) == nil
	if err := validateMySQLOptionFinalizationCatalog(db, artifacts.BackupTable); err != nil {
		return err
	}
	if !schemaMatches || !activeSchemaMatches {
		return fmt.Errorf("MySQL options schema changed during migration")
	}

	var triggerCount int64
	if err := db.Raw(`
SELECT count(*)
FROM information_schema.triggers
WHERE trigger_schema = DATABASE() AND event_object_table = ?`,
		artifacts.BackupTable).Scan(&triggerCount).Error; err != nil {
		return fmt.Errorf("inspect MySQL options schema metadata: %w", err)
	}
	triggers, err := inspectOwnedMySQLOptionMigrationTriggers(db, artifacts)
	if err != nil {
		return err
	}
	triggersMatch := int64(len(triggers)) == triggerCount &&
		!slices.ContainsFunc(triggers, func(trigger mysqlOptionTrigger) bool {
			return trigger.TableName != artifacts.BackupTable
		})

	expectedTriggers, validPhase := mysqlOptionMigrationBridgeNamesForPhase(artifacts, bridgePhase)
	if !validPhase {
		return fmt.Errorf("options migration recovery incomplete: invalid phase")
	}
	if !triggersMatch ||
		triggerCount != int64(len(expectedTriggers)) ||
		len(triggers) != len(expectedTriggers) {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	actualNames := make([]string, len(triggers))
	for i, trigger := range triggers {
		actualNames[i] = trigger.Name
	}
	slices.Sort(actualNames)
	slices.Sort(expectedTriggers)
	if !slices.Equal(actualNames, expectedTriggers) {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	return nil
}

func mysqlOptionMigrationBridgeNamesForPhase(
	artifacts mysqlOptionMigrationArtifacts,
	phase string,
) ([]string, bool) {
	switch phase {
	case optionMigrationPrepared, optionMigrationValidated:
		return artifacts.triggerNames(), true
	case optionMigrationInsertGone:
		return []string{artifacts.UpdateTrigger, artifacts.DeleteTrigger}, true
	case optionMigrationUpdateGone:
		return []string{artifacts.DeleteTrigger}, true
	case optionMigrationBridgesGone:
		return nil, true
	default:
		return nil, false
	}
}

func validMySQLOptionMigrationPhase(phase string) bool {
	switch phase {
	case optionMigrationPrepared, optionMigrationValidated,
		optionMigrationInsertGone, optionMigrationUpdateGone,
		optionMigrationBridgesGone:
		return true
	default:
		return false
	}
}

func parseMySQLOptionForwardIntent(phase string) (mysqlOptionForwardIntent, bool) {
	value, ok := strings.CutPrefix(phase, optionForwardIntentPrefix)
	if !ok {
		return mysqlOptionForwardIntent{}, false
	}
	targetCode, operation, ok := strings.Cut(value, "/")
	if !ok || strings.Contains(operation, "/") {
		return mysqlOptionForwardIntent{}, false
	}
	intent := mysqlOptionForwardIntent{
		TargetPhase: mysqlOptionMigrationPhaseFromCode(targetCode),
		Operation:   operation,
	}
	switch operation {
	case optionForwardDropInsert:
		if intent.TargetPhase != optionMigrationInsertGone {
			return mysqlOptionForwardIntent{}, false
		}
	case optionForwardDropUpdate:
		if intent.TargetPhase != optionMigrationUpdateGone {
			return mysqlOptionForwardIntent{}, false
		}
	case optionForwardDropDelete:
		if intent.TargetPhase != optionMigrationBridgesGone {
			return mysqlOptionForwardIntent{}, false
		}
	default:
		return mysqlOptionForwardIntent{}, false
	}
	return intent, intent.phase() == phase
}

func parseMySQLOptionRestoreIntent(phase string) (mysqlOptionRestoreIntent, bool) {
	value, ok := strings.CutPrefix(phase, optionRestoreIntentPrefix)
	if !ok {
		return mysqlOptionRestoreIntent{}, false
	}
	targetCode, operation, ok := strings.Cut(value, "/")
	if !ok || strings.Contains(operation, "/") {
		return mysqlOptionRestoreIntent{}, false
	}
	targetPhase := mysqlOptionMigrationPhaseFromCode(targetCode)
	if targetPhase == "" {
		return mysqlOptionRestoreIntent{}, false
	}
	intent := mysqlOptionRestoreIntent{
		TargetPhase: targetPhase,
		Operation:   operation,
	}
	switch operation {
	case optionRestoreCreateDelete:
		if targetPhase == optionMigrationBridgesGone {
			return mysqlOptionRestoreIntent{}, false
		}
	case optionRestoreCreateUpdate:
		if targetPhase == optionMigrationUpdateGone ||
			targetPhase == optionMigrationBridgesGone {
			return mysqlOptionRestoreIntent{}, false
		}
	case optionRestoreCreateInsert:
		if targetPhase != optionMigrationPrepared &&
			targetPhase != optionMigrationValidated {
			return mysqlOptionRestoreIntent{}, false
		}
	case optionRestoreClear:
	default:
		return mysqlOptionRestoreIntent{}, false
	}
	return intent, intent.phase() == phase
}

func nextMySQLOptionRestoreOperation(targetPhase, bridgePhase string) (string, bool) {
	if !validMySQLOptionMigrationPhase(targetPhase) {
		return "", false
	}
	if bridgePhase == targetPhase ||
		bridgePhase == optionMigrationPrepared && targetPhase == optionMigrationValidated {
		return optionRestoreClear, true
	}
	switch bridgePhase {
	case optionMigrationBridgesGone:
		if targetPhase != optionMigrationBridgesGone {
			return optionRestoreCreateDelete, true
		}
	case optionMigrationUpdateGone:
		switch targetPhase {
		case optionMigrationPrepared, optionMigrationValidated, optionMigrationInsertGone:
			return optionRestoreCreateUpdate, true
		}
	case optionMigrationInsertGone:
		if targetPhase == optionMigrationPrepared || targetPhase == optionMigrationValidated {
			return optionRestoreCreateInsert, true
		}
	}
	return "", false
}

func mysqlOptionRestoreOperationBridgePhases(
	intent mysqlOptionRestoreIntent,
) (string, string, bool) {
	switch intent.Operation {
	case optionRestoreCreateDelete:
		return optionMigrationBridgesGone, optionMigrationUpdateGone, true
	case optionRestoreCreateUpdate:
		return optionMigrationUpdateGone, optionMigrationInsertGone, true
	case optionRestoreCreateInsert:
		return optionMigrationInsertGone, intent.TargetPhase, true
	case optionRestoreClear:
		return intent.TargetPhase, intent.TargetPhase, true
	default:
		return "", "", false
	}
}

func mysqlOptionForwardOperation(
	currentPhase string,
) (mysqlOptionForwardIntent, bool) {
	switch currentPhase {
	case optionMigrationValidated:
		return mysqlOptionForwardIntent{
			TargetPhase: optionMigrationInsertGone,
			Operation:   optionForwardDropInsert,
		}, true
	case optionMigrationInsertGone:
		return mysqlOptionForwardIntent{
			TargetPhase: optionMigrationUpdateGone,
			Operation:   optionForwardDropUpdate,
		}, true
	case optionMigrationUpdateGone:
		return mysqlOptionForwardIntent{
			TargetPhase: optionMigrationBridgesGone,
			Operation:   optionForwardDropDelete,
		}, true
	default:
		return mysqlOptionForwardIntent{}, false
	}
}

func mysqlOptionForwardOperationBridgePhases(
	intent mysqlOptionForwardIntent,
) (string, string, string, bool) {
	switch intent.Operation {
	case optionForwardDropInsert:
		if intent.TargetPhase == optionMigrationInsertGone {
			return optionMigrationPrepared, optionMigrationInsertGone, "insert", true
		}
	case optionForwardDropUpdate:
		if intent.TargetPhase == optionMigrationUpdateGone {
			return optionMigrationInsertGone, optionMigrationUpdateGone, "update", true
		}
	case optionForwardDropDelete:
		if intent.TargetPhase == optionMigrationBridgesGone {
			return optionMigrationUpdateGone, optionMigrationBridgesGone, "delete", true
		}
	}
	return "", "", "", false
}

func validateMySQLOptionIncomingForeignKeys(db *gorm.DB, tables ...string) error {
	if len(tables) == 0 ||
		slices.ContainsFunc(tables, func(table string) bool { return !optionSafeIdent(table) }) {
		return fmt.Errorf("inspect MySQL options schema metadata: unsafe table name")
	}
	if err := validateMySQLGlobalTableMetadataVisibility(db); err != nil {
		return err
	}
	var metadata struct {
		HasIncomingForeignKey bool `gorm:"column:has_incoming_foreign_key"`
	}
	if err := db.Raw(`
SELECT EXISTS (
  SELECT 1
  FROM information_schema.key_column_usage
  WHERE referenced_table_schema = DATABASE()
    AND referenced_table_name IN ?
) AS has_incoming_foreign_key`, tables).Scan(&metadata).Error; err != nil {
		return fmt.Errorf("inspect MySQL options schema metadata: %w", err)
	}
	if metadata.HasIncomingForeignKey {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	return nil
}

func validateMySQLGlobalTableMetadataVisibility(db *gorm.DB) error {
	var metadata struct {
		HasGlobalPrivilege bool `gorm:"column:has_global_privilege"`
	}
	if err := db.Raw(`
SELECT EXISTS (
  SELECT 1
  FROM information_schema.user_privileges
  WHERE grantee = CONCAT(
    QUOTE(LEFT(
      CURRENT_USER(),
      LENGTH(CURRENT_USER()) - LENGTH(SUBSTRING_INDEX(CURRENT_USER(), '@', -1)) - 1
    )),
    '@',
    QUOTE(SUBSTRING_INDEX(CURRENT_USER(), '@', -1))
  )
    AND privilege_type IN ('REFERENCES', 'SELECT')
) AS has_global_privilege`).Scan(&metadata).Error; err != nil {
		return errMySQLMetadataVisibilityInspection
	}

	rows, err := db.Raw("SHOW GRANTS FOR CURRENT_USER").Rows()
	if err != nil {
		return errMySQLMetadataVisibilityInspection
	}
	defer rows.Close()
	showGrantsCapability := false
	for rows.Next() {
		var grant string
		if err := rows.Scan(&grant); err != nil {
			return errMySQLMetadataVisibilityInspection
		}
		if mysqlGrantProvidesGlobalTableMetadataVisibility(grant) {
			showGrantsCapability = true
		}
	}
	if err := rows.Err(); err != nil {
		return errMySQLMetadataVisibilityInspection
	}
	if !metadata.HasGlobalPrivilege || !showGrantsCapability {
		return errMySQLGlobalMetadataVisibilityRequired
	}
	return nil
}

func mysqlGrantProvidesGlobalTableMetadataVisibility(grant string) bool {
	normalized := strings.ToUpper(strings.Join(strings.Fields(grant), " "))
	privileges, _, ok := strings.Cut(normalized, " ON *.* TO ")
	if !ok {
		return false
	}
	privileges, ok = strings.CutPrefix(privileges, "GRANT ")
	if !ok || privileges == "" {
		return false
	}
	if privileges == "ALL PRIVILEGES" {
		return true
	}
	for privilege := range strings.SplitSeq(privileges, ",") {
		switch strings.TrimSpace(privilege) {
		case "REFERENCES", "SELECT":
			return true
		}
	}
	return false
}

func validateMySQLOptionFinalizationCatalog(db *gorm.DB, retained string) error {
	var metadata struct {
		TriggerCount int64 `gorm:"column:trigger_count"`
	}
	if err := db.Raw(`
SELECT
  (SELECT count(*)
   FROM information_schema.triggers
   WHERE trigger_schema = DATABASE() AND event_object_table = 'options') AS trigger_count`,
	).Scan(&metadata).Error; err != nil {
		return fmt.Errorf("inspect active MySQL options schema metadata: %w", err)
	}
	if metadata.TriggerCount != 0 {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	return validateMySQLOptionIncomingForeignKeys(db, "options", retained)
}

func inspectMySQLOptionMigrationState(
	db *gorm.DB,
	table string,
) (mysqlOptionMigrationState, bool, error) {
	if !optionSafeIdent(table) {
		return mysqlOptionMigrationState{}, false, fmt.Errorf("inspect MySQL migration state: unsafe table name")
	}
	var states []struct {
		DataType     string         `gorm:"column:data_type"`
		ColumnType   string         `gorm:"column:column_type"`
		Nullable     string         `gorm:"column:is_nullable"`
		DefaultValue sql.NullString `gorm:"column:column_default"`
		Comment      string         `gorm:"column:column_comment"`
	}
	if err := db.Raw(`
SELECT data_type, column_type, is_nullable, column_default, column_comment
FROM information_schema.columns
WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`,
		table, optionMigrationStateCol).Scan(&states).Error; err != nil {
		return mysqlOptionMigrationState{}, false, fmt.Errorf("inspect MySQL migration state: %w", err)
	}
	if len(states) == 0 {
		return mysqlOptionMigrationState{}, false, nil
	}
	if len(states) != 1 || states[0].DataType != "varchar" ||
		!strings.EqualFold(states[0].ColumnType, fmt.Sprintf("varchar(%d)", optionMigrationStateSize)) ||
		!strings.EqualFold(states[0].Nullable, "NO") ||
		states[0].Comment != optionMigrationStateMarker ||
		!states[0].DefaultValue.Valid {
		return mysqlOptionMigrationState{}, false,
			fmt.Errorf("options migration artifact collision: state column=%s", optionMigrationStateCol)
	}
	parts := strings.Split(states[0].DefaultValue.String, ":")
	_, restoreIntent := parseMySQLOptionRestoreIntent(parts[0])
	_, forwardIntent := parseMySQLOptionForwardIntent(parts[0])
	if len(parts) != 3 ||
		(!validMySQLOptionMigrationPhase(parts[0]) && !restoreIntent && !forwardIntent) ||
		!validOptionMigrationSchemaDigest(parts[1]) ||
		!validOptionMigrationSchemaDigest(parts[2]) {
		return mysqlOptionMigrationState{}, false,
			fmt.Errorf("options migration artifact collision: state column=%s", optionMigrationStateCol)
	}
	return mysqlOptionMigrationState{
		Phase:              parts[0],
		SourceSchemaSHA256: parts[1],
		ActiveSchemaSHA256: parts[2],
	}, true, nil
}

func validOptionMigrationSchemaDigest(digest string) bool {
	return len(digest) == sha256.Size*2 &&
		!strings.ContainsFunc(digest, func(r rune) bool {
			return r < '0' || r > '9' && r < 'a' || r > 'f'
		})
}

func recordMySQLOptionActiveSchema(
	db *gorm.DB,
	artifacts *mysqlOptionMigrationArtifacts,
) error {
	state, hasState, err := inspectMySQLOptionMigrationState(db, artifacts.TmpTable)
	if err != nil {
		return err
	}
	zeroDigest := strings.Repeat("0", sha256.Size*2)
	if !hasState || state.Phase != optionMigrationPrepared ||
		state.SourceSchemaSHA256 != artifacts.SourceSchemaSHA256 ||
		state.ActiveSchemaSHA256 != zeroDigest {
		return fmt.Errorf("options migration artifact collision: invalid prepared state")
	}
	digest, err := mysqlOptionActiveSchemaDigest(db, artifacts.TmpTable, state)
	if err != nil {
		return err
	}
	artifacts.ActiveSchemaSHA256 = digest
	state.ActiveSchemaSHA256 = digest
	if err := db.Exec(
		"ALTER TABLE " + quoteMySQLIdent(artifacts.TmpTable) + " ALTER COLUMN " +
			quoteMySQLIdent(optionMigrationStateCol) + " SET DEFAULT '" + state.value() + "'",
	).Error; err != nil {
		return fmt.Errorf("record options replacement schema: %w", err)
	}
	return validateMySQLOptionActiveSchema(db, artifacts.TmpTable, *artifacts, state)
}

func validateMySQLOptionActiveSchema(
	db *gorm.DB,
	table string,
	artifacts mysqlOptionMigrationArtifacts,
	state mysqlOptionMigrationState,
) error {
	owned, err := hasOwnedMySQLOptionMigrationMarker(db, table, artifacts.Marker)
	if err != nil {
		return err
	}
	if !owned ||
		state.SourceSchemaSHA256 != artifacts.SourceSchemaSHA256 ||
		state.ActiveSchemaSHA256 != artifacts.ActiveSchemaSHA256 {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	digest, err := mysqlOptionActiveSchemaDigest(db, table, state)
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(digest), []byte(artifacts.ActiveSchemaSHA256)) {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	primary, err := mysqlOptionKeyIsSolePrimary(db, table)
	if err != nil {
		return err
	}
	if !primary {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	return nil
}

func mysqlOptionActiveSchemaDigest(
	db *gorm.DB,
	table string,
	state mysqlOptionMigrationState,
) (string, error) {
	ddl, err := showCreateMySQLTable(db, table)
	if err != nil {
		return "", err
	}
	normalized, ok := replaceMySQLCreateTableName(ddl, table, "options")
	if !ok {
		return "", fmt.Errorf("MySQL options schema changed during migration")
	}
	stateDefault := "DEFAULT '" + state.value() + "'"
	if strings.Count(normalized, stateDefault) != 1 {
		return "", fmt.Errorf("MySQL options schema changed during migration")
	}
	normalized = strings.Replace(normalized, stateDefault, "DEFAULT '<migration-state>'", 1)
	digest := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(digest[:]), nil
}

func mysqlOptionTerminalSchemaDigest(db *gorm.DB, table string) (string, error) {
	ddl, err := showCreateMySQLTable(db, table)
	if err != nil {
		return "", err
	}
	normalized, ok := replaceMySQLCreateTableName(ddl, table, "options")
	if !ok {
		return "", fmt.Errorf("MySQL options schema changed during migration")
	}
	terminalDDL, ok := removeMySQLCreateTableColumns(
		normalized, optionMigrationMarkerCol, optionMigrationStateCol,
	)
	if !ok {
		return "", fmt.Errorf("MySQL options schema changed during migration")
	}
	digest := sha256.Sum256([]byte(terminalDDL))
	return hex.EncodeToString(digest[:]), nil
}

func mysqlOptionCurrentSchemaDigest(db *gorm.DB, table string) (string, error) {
	ddl, err := showCreateMySQLTable(db, table)
	if err != nil {
		return "", err
	}
	normalized, ok := replaceMySQLCreateTableName(ddl, table, "options")
	if !ok {
		return "", fmt.Errorf("MySQL options schema changed during migration")
	}
	digest := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(digest[:]), nil
}

func mysqlOptionRowsDigest(db *gorm.DB, table string, columns []string) (string, error) {
	if !optionSafeIdent(table) || len(columns) == 0 ||
		slices.ContainsFunc(columns, func(column string) bool {
			return !optionSafeIdent(column)
		}) {
		return "", fmt.Errorf("inspect MySQL options rows: unsafe identifier")
	}
	return mysqlOptionProjectedRowsDigest(
		db, table, columns, mysqlOptionDigestSelects(columns), false,
	)
}

func mysqlOptionSemanticRowsDigest(db *gorm.DB, table string, columns []string) (string, error) {
	if !optionSafeIdent(table) || len(columns) == 0 ||
		!slices.Contains(columns, "key") ||
		slices.ContainsFunc(columns, func(column string) bool {
			return !optionSafeIdent(column)
		}) {
		return "", fmt.Errorf("inspect semantic MySQL options rows: unsafe projection")
	}
	return mysqlOptionProjectedRowsDigest(
		db, table, columns, mysqlOptionSemanticDigestSelects(columns), true,
	)
}

func mysqlOptionProjectedRowsDigest(
	db *gorm.DB,
	table string,
	columns []string,
	selects []string,
	semantic bool,
) (string, error) {
	rows, err := db.Raw(
		"SELECT "+strings.Join(selects, ", ")+" FROM "+quoteMySQLIdent(table)+
			" LIMIT ?",
		optionMigrationMaxRows+1,
	).Rows()
	if err != nil {
		return "", fmt.Errorf("inspect MySQL options rows")
	}
	defer rows.Close()

	encodedRows := make([][]byte, 0)
	for rows.Next() {
		encoded, err := scanMySQLOptionDigestRow(rows, len(columns))
		if err != nil {
			return "", fmt.Errorf("inspect MySQL options rows")
		}
		encodedRows = append(encodedRows, encoded)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("inspect MySQL options rows")
	}
	if len(encodedRows) > optionMigrationMaxRows {
		return "", fmt.Errorf(
			"inspect MySQL options rows: row limit exceeded: maximum=%d",
			optionMigrationMaxRows,
		)
	}
	if semantic {
		return mysqlOptionSemanticDigestEncodedRows(columns, encodedRows), nil
	}
	return mysqlOptionDigestEncodedRows(columns, encodedRows), nil
}

func mysqlOptionCanonicalRowsSemanticDigest(
	db *gorm.DB,
	table string,
	columns []string,
) (string, error) {
	if !optionSafeIdent(table) || len(columns) == 0 ||
		slices.ContainsFunc(columns, func(column string) bool {
			return !optionSafeIdent(column)
		}) {
		return "", fmt.Errorf("inspect canonical MySQL options rows: unsafe identifier")
	}
	var sourceRowCount int64
	if err := db.Table(table).Count(&sourceRowCount).Error; err != nil {
		return "", fmt.Errorf("inspect canonical MySQL options rows")
	}
	if sourceRowCount > optionMigrationMaxRows {
		return "", fmt.Errorf(
			"inspect canonical MySQL options rows: row limit exceeded: maximum=%d",
			optionMigrationMaxRows,
		)
	}
	rows, err := readMySQLOptionMigrationRows(db, table, sourceRowCount)
	if err != nil {
		return "", fmt.Errorf("inspect canonical MySQL options rows")
	}
	canonical, err := dedupeOptionRows(rows, make([]byte, optionDigestKeySize))
	if err != nil {
		return "", fmt.Errorf("inspect canonical MySQL options rows")
	}
	selects := mysqlOptionSemanticDigestSelects(columns)
	encodedRows := make([][]byte, 0, len(canonical))
	for _, option := range canonical {
		row := db.Raw(
			"SELECT "+strings.Join(selects, ", ")+
				mysqlExactOptionRowSelection(table),
			len([]byte(option.Key)), option.Key,
		).Row()
		encoded, err := scanMySQLOptionDigestRow(row, len(columns))
		if err != nil {
			return "", fmt.Errorf("inspect canonical MySQL options rows")
		}
		encodedRows = append(encodedRows, encoded)
	}
	return mysqlOptionSemanticDigestEncodedRows(columns, encodedRows), nil
}

func mysqlOptionDigestSelects(columns []string) []string {
	selects := make([]string, 0, len(columns)*2)
	for _, column := range columns {
		quoted := quoteMySQLIdent(column)
		selects = append(selects, quoted+" IS NULL", "CAST("+quoted+" AS BINARY)")
	}
	return selects
}

func mysqlOptionSemanticDigestSelects(columns []string) []string {
	selects := make([]string, 0, len(columns)*2)
	for _, column := range columns {
		quoted := quoteMySQLIdent(column)
		if column == "key" {
			selects = append(selects, quoted+" IS NULL", "WEIGHT_STRING("+quoted+")")
			continue
		}
		selects = append(selects, quoted+" IS NULL", "CAST("+quoted+" AS BINARY)")
	}
	return selects
}

type mysqlOptionRowScanner interface {
	Scan(...any) error
}

func scanMySQLOptionDigestRow(row mysqlOptionRowScanner, columnCount int) ([]byte, error) {
	nulls := make([]bool, columnCount)
	values := make([][]byte, columnCount)
	destinations := make([]any, 0, columnCount*2)
	for i := range columnCount {
		destinations = append(destinations, &nulls[i], &values[i])
	}
	if err := row.Scan(destinations...); err != nil {
		return nil, err
	}
	encoded := make([]byte, 0)
	var length [8]byte
	for i := range columnCount {
		if nulls[i] {
			encoded = append(encoded, 0)
			continue
		}
		encoded = append(encoded, 1)
		binary.BigEndian.PutUint64(length[:], uint64(len(values[i])))
		encoded = append(encoded, length[:]...)
		encoded = append(encoded, values[i]...)
	}
	return encoded, nil
}

func mysqlOptionDigestEncodedRows(columns []string, encodedRows [][]byte) string {
	return mysqlOptionDigestEncodedRowsWithDomain(
		"new-api/options-terminal-rows/v1\x00", columns, encodedRows,
	)
}

func mysqlOptionSemanticDigestEncodedRows(columns []string, encodedRows [][]byte) string {
	return mysqlOptionDigestEncodedRowsWithDomain(
		"new-api/options-terminal-semantic-rows/v1\x00", columns, encodedRows,
	)
}

func mysqlOptionDigestEncodedRowsWithDomain(
	domain string,
	columns []string,
	encodedRows [][]byte,
) string {
	slices.SortFunc(encodedRows, bytes.Compare)
	hash := sha256.New()
	_, _ = hash.Write([]byte(domain))
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(columns)))
	_, _ = hash.Write(length[:])
	for _, column := range columns {
		binary.BigEndian.PutUint64(length[:], uint64(len(column)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(column))
	}
	binary.BigEndian.PutUint64(length[:], uint64(len(encodedRows)))
	_, _ = hash.Write(length[:])
	for _, row := range encodedRows {
		binary.BigEndian.PutUint64(length[:], uint64(len(row)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write(row)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

type mysqlOptionTerminalRowDigestSet struct {
	ActiveRaw                 string
	RetainedRaw               string
	ActiveSemantic            string
	RetainedCanonicalSemantic string
}

func mysqlOptionTerminalRowDigests(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
) (mysqlOptionTerminalRowDigestSet, error) {
	activeColumns, err := mysqlWritableOptionColumnsFromTable(db, "options")
	if err != nil {
		return mysqlOptionTerminalRowDigestSet{}, err
	}
	retainedColumns, err := mysqlWritableOptionColumnsFromTable(db, artifacts.BackupTable)
	if err != nil {
		return mysqlOptionTerminalRowDigestSet{}, err
	}
	if !slices.Equal(activeColumns, retainedColumns) {
		return mysqlOptionTerminalRowDigestSet{},
			fmt.Errorf("MySQL options row data changed during migration")
	}
	activeRaw, err := mysqlOptionRowsDigest(db, "options", activeColumns)
	if err != nil {
		return mysqlOptionTerminalRowDigestSet{}, err
	}
	retainedRaw, err := mysqlOptionRowsDigest(
		db, artifacts.BackupTable, retainedColumns,
	)
	if err != nil {
		return mysqlOptionTerminalRowDigestSet{}, err
	}
	activeSemantic, err := mysqlOptionSemanticRowsDigest(db, "options", activeColumns)
	if err != nil {
		return mysqlOptionTerminalRowDigestSet{}, err
	}
	retainedCanonicalSemantic, err := mysqlOptionCanonicalRowsSemanticDigest(
		db, artifacts.BackupTable, retainedColumns,
	)
	if err != nil {
		return mysqlOptionTerminalRowDigestSet{}, err
	}
	return mysqlOptionTerminalRowDigestSet{
		ActiveRaw:                 activeRaw,
		RetainedRaw:               retainedRaw,
		ActiveSemantic:            activeSemantic,
		RetainedCanonicalSemantic: retainedCanonicalSemantic,
	}, nil
}

func validateMySQLOptionTerminalRows(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	expectedActive string,
	expectedRetained string,
	expectedRetainedCanonical string,
) error {
	digests, err := mysqlOptionTerminalRowDigests(db, artifacts)
	if err != nil ||
		!hmac.Equal(
			[]byte(digests.ActiveSemantic),
			[]byte(digests.RetainedCanonicalSemantic),
		) ||
		!hmac.Equal([]byte(digests.ActiveRaw), []byte(expectedActive)) ||
		!hmac.Equal([]byte(digests.RetainedRaw), []byte(expectedRetained)) ||
		!hmac.Equal(
			[]byte(digests.RetainedCanonicalSemantic),
			[]byte(expectedRetainedCanonical),
		) {
		return fmt.Errorf("MySQL options row data changed during migration")
	}
	return nil
}

func removeMySQLCreateTableColumns(sourceDDL string, columns ...string) (string, bool) {
	position := skipMySQLSpace(sourceDDL, 0)
	next, ok := consumeMySQLKeyword(sourceDDL, position, "CREATE")
	if !ok {
		return "", false
	}
	position = skipMySQLSpace(sourceDDL, next)
	next, ok = consumeMySQLKeyword(sourceDDL, position, "TABLE")
	if !ok {
		return "", false
	}
	identifierStart := skipMySQLSpace(sourceDDL, next)
	_, identifierEnd, ok := parseMySQLQuotedIdentifier(sourceDDL, identifierStart)
	if !ok {
		return "", false
	}
	open := skipMySQLSpace(sourceDDL, identifierEnd)
	if open >= len(sourceDDL) || sourceDDL[open] != '(' {
		return "", false
	}
	close, ok := matchingMySQLDDLParen(sourceDDL, open)
	if !ok {
		return "", false
	}
	definitions, ok := splitMySQLDDLDefinitions(sourceDDL[open+1 : close])
	if !ok {
		return "", false
	}
	remove := make(map[string]bool, len(columns))
	for _, column := range columns {
		if _, duplicate := remove[column]; !optionSafeIdent(column) || duplicate {
			return "", false
		}
		remove[column] = false
	}
	kept := make([]string, 0, max(0, len(definitions)-len(columns)))
	for _, definition := range definitions {
		trimmed := strings.TrimSpace(definition)
		name, _, isColumn := parseMySQLQuotedIdentifier(trimmed, 0)
		if isColumn {
			if found, targeted := remove[name]; targeted {
				if found {
					return "", false
				}
				remove[name] = true
				continue
			}
		}
		kept = append(kept, definition)
	}
	if slices.ContainsFunc(columns, func(column string) bool { return !remove[column] }) {
		return "", false
	}
	return sourceDDL[:open+1] + strings.Join(kept, ",") + sourceDDL[close:], true
}

func splitMySQLDDLDefinitions(body string) ([]string, bool) {
	definitions := make([]string, 0, 8)
	start := 0
	depth := 0
	quote := byte(0)
	lineComment := false
	blockComment := false
	for i := 0; i < len(body); i++ {
		current := body[i]
		if lineComment {
			if current == '\n' {
				lineComment = false
			}
			continue
		}
		if blockComment {
			if current == '*' && i+1 < len(body) && body[i+1] == '/' {
				blockComment = false
				i++
			}
			continue
		}
		if quote != 0 {
			if current == '\\' && quote != '`' {
				i++
				if i >= len(body) {
					return nil, false
				}
				continue
			}
			if current != quote {
				continue
			}
			if i+1 < len(body) && body[i+1] == quote {
				i++
				continue
			}
			quote = 0
			continue
		}
		switch current {
		case '\'', '"', '`':
			quote = current
		case '#':
			lineComment = true
		case '-':
			if i+2 < len(body) && body[i+1] == '-' &&
				(body[i+2] == ' ' || body[i+2] == '\t' ||
					body[i+2] == '\r' || body[i+2] == '\n') {
				lineComment = true
				i++
			}
		case '/':
			if i+1 < len(body) && body[i+1] == '*' {
				blockComment = true
				i++
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil, false
			}
		case ',':
			if depth == 0 {
				definitions = append(definitions, body[start:i])
				start = i + 1
			}
		}
	}
	if quote != 0 || blockComment || depth != 0 {
		return nil, false
	}
	definitions = append(definitions, body[start:])
	return definitions, true
}

func mysqlOptionKeyIsSolePrimary(db *gorm.DB, table string) (bool, error) {
	if !optionSafeIdent(table) {
		return false, fmt.Errorf("inspect MySQL options primary key: unsafe table name")
	}
	var columns []string
	if err := db.Raw(`
SELECT column_name
FROM information_schema.key_column_usage
WHERE constraint_schema = DATABASE()
  AND table_name = ?
  AND constraint_name = 'PRIMARY'
ORDER BY ordinal_position`, table).Scan(&columns).Error; err != nil {
		return false, fmt.Errorf("inspect MySQL options primary key: %w", err)
	}
	return len(columns) == 1 && columns[0] == "key", nil
}

func finalizeMySQLOptionMigration(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	phase string,
) error {
	return finalizeMySQLOptionMigrationWithJournal(db, artifacts, phase, nil)
}

func finalizeMySQLOptionMigrationWithJournal(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	phase string,
	existingJournal *mysqlOptionTerminalJournal,
) (resultErr error) {
	if !optionSafeIdent(artifacts.BackupTable) {
		return fmt.Errorf("finalize options migration: unsafe retained table name")
	}
	includeJournal := existingJournal != nil
	if err := lockMySQLOptionFinalizationTables(
		db, artifacts.BackupTable, includeJournal,
	); err != nil {
		return err
	}

	state, err := readAndValidateMySQLOptionFinalizationState(
		db, artifacts, phase, phase,
	)
	if err != nil {
		return err
	}
	retainedTriggers, err := inspectOwnedMySQLOptionMigrationTriggers(db, artifacts)
	if err != nil {
		return err
	}
	initialState := state
	restorePoint := mysqlOptionFinalizationRestorePoint{
		StatePhase: state.Phase, BridgePhase: state.Phase,
	}
	mutated := false
	durableCompletion := includeJournal
	defer func() {
		if resultErr == nil {
			return
		}
		if durableCompletion {
			resultErr = mysqlOptionTerminalJournalRecoveryBlocked(resultErr)
			return
		}
		if !mutated {
			return
		}
		if restoreErr := restoreMySQLOptionFinalizationState(
			db, artifacts, initialState, retainedTriggers, restorePoint,
		); restoreErr != nil {
			resultErr = errors.Join(resultErr, restoreErr)
		}
	}()

	if state.Phase == optionMigrationPrepared {
		if err := setMySQLOptionMigrationPhase(
			db, state, optionMigrationValidated,
		); err != nil {
			return err
		}
		mutated = true
		restorePoint.StatePhase = optionMigrationValidated
		if err := lockMySQLOptionFinalizationTables(
			db, artifacts.BackupTable, false,
		); err != nil {
			return err
		}
		state, err = readAndValidateMySQLOptionFinalizationState(
			db, artifacts, optionMigrationValidated, optionMigrationValidated,
		)
		if err != nil {
			return err
		}
	}

	terminalSchemaDigest, err := mysqlOptionTerminalSchemaDigest(db, "options")
	if err != nil {
		return err
	}
	restorePoint.TerminalSchemaSHA256 = terminalSchemaDigest
	journal := mysqlOptionTerminalJournal{}
	if existingJournal != nil {
		journal = *existingJournal
		if err := validateMySQLOptionTerminalJournalSnapshot(
			db, artifacts, journal,
		); err != nil {
			return err
		}
	} else {
		journal = mysqlOptionTerminalJournal{
			ID:                   artifacts.ID,
			Phase:                optionMigrationBridgesGone,
			SourceSchemaSHA256:   state.SourceSchemaSHA256,
			TerminalSchemaSHA256: terminalSchemaDigest,
		}
		if err := db.Exec("UNLOCK TABLES").Error; err != nil {
			return fmt.Errorf("unlock options finalization tables: %w", err)
		}
		durableCompletion = true
		if err := createMySQLOptionTerminalJournal(db, journal); err != nil {
			return err
		}
		includeJournal = true
		if err := lockMySQLOptionFinalizationTables(
			db, artifacts.BackupTable, true,
		); err != nil {
			return err
		}
		state, err = readAndValidateMySQLOptionFinalizationState(
			db, artifacts, state.Phase, state.Phase,
		)
		if err != nil {
			return err
		}
		journal, err = persistMySQLOptionTerminalJournalSnapshot(
			db, artifacts, journal,
		)
		if err != nil {
			return err
		}
	}

	for state.Phase != optionMigrationBridgesGone {
		intent, valid := mysqlOptionForwardOperation(state.Phase)
		if !valid {
			return fmt.Errorf("options migration recovery incomplete: invalid phase")
		}
		preBridgePhase, nextPhase, triggerKind, _ :=
			mysqlOptionForwardOperationBridgePhases(intent)
		trigger := artifacts.InsertTrigger
		switch triggerKind {
		case "update":
			trigger = artifacts.UpdateTrigger
		case "delete":
			trigger = artifacts.DeleteTrigger
		}

		if err := lockMySQLOptionFinalizationTables(
			db, artifacts.BackupTable, includeJournal,
		); err != nil {
			return err
		}
		state, err = readAndValidateMySQLOptionFinalizationState(
			db, artifacts, state.Phase, state.Phase,
		)
		if err != nil {
			return err
		}
		if err := validateMySQLOptionTerminalJournalSnapshot(
			db, artifacts, journal,
		); err != nil {
			return err
		}
		if err := setMySQLOptionMigrationPhase(db, state, intent.phase()); err != nil {
			return err
		}
		mutated = true
		restorePoint.StatePhase = intent.phase()

		intentState := state
		intentState.Phase = nextPhase
		bridgePhase, err := inspectMySQLOptionForwardIntentState(
			db, artifacts, intentState, intent, includeJournal,
		)
		if err != nil {
			return err
		}
		if err := validateMySQLOptionTerminalJournalSnapshot(
			db, artifacts, journal,
		); err != nil {
			return err
		}
		if bridgePhase != preBridgePhase {
			return errMySQLOptionFinalizationRecoveryBlocked
		}
		if err := db.Exec("DROP TRIGGER " + quoteMySQLIdent(trigger)).Error; err != nil {
			return fmt.Errorf("drop options migration trigger %s: %w", trigger, err)
		}
		restorePoint.BridgePhase = nextPhase

		bridgePhase, err = inspectMySQLOptionForwardIntentState(
			db, artifacts, intentState, intent, includeJournal,
		)
		if err != nil {
			return err
		}
		if err := validateMySQLOptionTerminalJournalSnapshot(
			db, artifacts, journal,
		); err != nil {
			return err
		}
		if bridgePhase != nextPhase {
			return errMySQLOptionFinalizationRecoveryBlocked
		}
		if err := setMySQLOptionMigrationPhase(db, state, nextPhase); err != nil {
			return err
		}
		restorePoint.StatePhase = nextPhase

		if err := lockMySQLOptionFinalizationTables(
			db, artifacts.BackupTable, includeJournal,
		); err != nil {
			return err
		}
		state, err = readAndValidateMySQLOptionFinalizationState(
			db, artifacts, nextPhase, nextPhase,
		)
		if err != nil {
			return err
		}
		if err := validateMySQLOptionTerminalJournalSnapshot(
			db, artifacts, journal,
		); err != nil {
			return err
		}
	}

	if err := validateMarkedMySQLOptionTerminalJournal(
		db, artifacts, state, journal,
	); err != nil {
		return err
	}
	if err := db.Exec(
		"ALTER TABLE `options` DROP COLUMN " + quoteMySQLIdent(optionMigrationStateCol) +
			", DROP COLUMN " + quoteMySQLIdent(optionMigrationMarkerCol),
	).Error; err != nil {
		return fmt.Errorf("drop options migration markers")
	}
	restorePoint.MarkersDropped = true
	if err := lockMySQLOptionTerminalTables(db, artifacts.BackupTable, true); err != nil {
		return err
	}
	if err := validateTerminalMySQLOptionMigration(
		db, artifacts, state, terminalSchemaDigest,
		journal.ActiveRowsSHA256, journal.RetainedRowsSHA256,
		journal.RetainedCanonicalRowsSHA256,
	); err != nil {
		return err
	}
	if err := clearMySQLOptionTerminalJournal(db, artifacts, state, journal); err != nil {
		return err
	}
	return nil
}

func readAndValidateMySQLOptionFinalizationState(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	expectedPhase string,
	expectedBridgePhase string,
) (mysqlOptionMigrationState, error) {
	state, hasState, err := inspectMySQLOptionMigrationState(db, "options")
	if err != nil {
		return mysqlOptionMigrationState{}, err
	}
	if !hasState || state.Phase != expectedPhase ||
		!hmac.Equal([]byte(state.SourceSchemaSHA256), []byte(artifacts.SourceSchemaSHA256)) ||
		!hmac.Equal([]byte(state.ActiveSchemaSHA256), []byte(artifacts.ActiveSchemaSHA256)) {
		return mysqlOptionMigrationState{},
			fmt.Errorf("options migration artifact collision: invalid finalization state")
	}
	if err := validateMySQLOptionFinalizationStateForBridgePhase(
		db, artifacts, state, expectedBridgePhase,
	); err != nil {
		return mysqlOptionMigrationState{}, err
	}
	return state, nil
}

func setMySQLOptionMigrationPhase(
	db *gorm.DB,
	state mysqlOptionMigrationState,
	nextPhase string,
) error {
	state.Phase = nextPhase
	if err := db.Exec(
		"ALTER TABLE `options` ALTER COLUMN " + quoteMySQLIdent(optionMigrationStateCol) +
			" SET DEFAULT '" + state.value() + "'",
	).Error; err != nil {
		return fmt.Errorf("mark options migration phase %s: %w", nextPhase, err)
	}
	return nil
}

func validateTerminalMySQLOptionMigration(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	state mysqlOptionMigrationState,
	expectedActiveDigest string,
	expectedActiveRows string,
	expectedRetainedRows string,
	expectedRetainedCanonicalRows string,
) error {
	if err := validateMySQLGlobalTableMetadataVisibility(db); err != nil {
		return err
	}
	activeDigest, err := mysqlOptionCurrentSchemaDigest(db, "options")
	if err != nil || !hmac.Equal([]byte(activeDigest), []byte(expectedActiveDigest)) {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	primary, err := mysqlOptionKeyIsSolePrimary(db, "options")
	if err != nil {
		return err
	}
	if !primary {
		return fmt.Errorf("MySQL options schema changed during migration")
	}

	retainedDDL, err := showCreateMySQLTable(db, artifacts.BackupTable)
	if err != nil {
		return fmt.Errorf("inspect retained MySQL options schema: %w", err)
	}
	normalizedRetained, ok := replaceMySQLCreateTableName(
		retainedDDL, artifacts.BackupTable, "options",
	)
	if !ok {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	retainedDigest := sha256.Sum256([]byte(normalizedRetained))
	if !hmac.Equal(
		[]byte(hex.EncodeToString(retainedDigest[:])),
		[]byte(state.SourceSchemaSHA256),
	) {
		return fmt.Errorf("MySQL options schema changed during migration")
	}

	_, hasState, err := inspectMySQLOptionMigrationState(db, "options")
	if err != nil {
		return err
	}
	owned, err := hasOwnedMySQLOptionMigrationMarker(db, "options", artifacts.Marker)
	if err != nil {
		return err
	}
	if hasState || owned {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	var triggerCount int64
	if err := db.Raw(`
SELECT count(*)
FROM information_schema.triggers
WHERE trigger_schema = DATABASE()
  AND event_object_table IN ('options', ?)`,
		artifacts.BackupTable).Scan(&triggerCount).Error; err != nil {
		return fmt.Errorf("inspect MySQL options schema metadata: %w", err)
	}
	candidates, err := inspectMySQLOptionMigrationTriggerCandidates(db, artifacts)
	if err != nil {
		return err
	}
	hasCandidateTriggers, err := hasMySQLOptionMigrationTriggers(db)
	if err != nil {
		return err
	}
	if triggerCount != 0 || len(candidates) != 0 || hasCandidateTriggers {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	if err := validateMySQLOptionIncomingForeignKeys(
		db, "options", artifacts.BackupTable,
	); err != nil {
		return err
	}
	if expectedActiveRows == "" &&
		expectedRetainedRows == "" &&
		expectedRetainedCanonicalRows == "" {
		return nil
	}
	return validateMySQLOptionTerminalRows(
		db, artifacts, expectedActiveRows, expectedRetainedRows,
		expectedRetainedCanonicalRows,
	)
}

func recoverMySQLOptionTerminalJournal(db *gorm.DB) (bool, error) {
	journal, exists, err := inspectMySQLOptionTerminalJournal(db)
	if err != nil || !exists {
		return false, err
	}
	artifacts := mysqlOptionMigrationArtifacts{
		ID:            journal.ID,
		Marker:        optionMigrationMarker + "/" + journal.ID,
		TmpTable:      optionMySQLTmpPrefix + journal.ID,
		BackupTable:   optionLegacyTablePrefix + journal.ID,
		InsertTrigger: optionInsertTriggerPrefix + journal.ID,
		UpdateTrigger: optionUpdateTriggerPrefix + journal.ID,
		DeleteTrigger: optionDeleteTriggerPrefix + journal.ID,
	}
	if err := lockMySQLOptionTerminalTables(db, artifacts.BackupTable, true); err != nil {
		return false, err
	}
	state, hasState, err := inspectMySQLOptionMigrationState(db, "options")
	if err != nil {
		return false, err
	}
	owned, err := hasOwnedMySQLOptionMigrationMarker(db, "options", artifacts.Marker)
	if err != nil {
		return false, err
	}
	if hasState != owned {
		return false, fmt.Errorf("options migration artifact collision: incomplete terminal ownership")
	}
	if hasState {
		artifacts.SourceSchemaSHA256 = state.SourceSchemaSHA256
		artifacts.ActiveSchemaSHA256 = state.ActiveSchemaSHA256
		if !journal.hasSnapshot() {
			if state.Phase != optionMigrationValidated {
				return false, fmt.Errorf(
					"options migration artifact collision: incomplete completion journal",
				)
			}
			state, err = readAndValidateMySQLOptionFinalizationState(
				db, artifacts, optionMigrationValidated, optionMigrationValidated,
			)
			if err != nil {
				return false, err
			}
			journal, err = persistMySQLOptionTerminalJournalSnapshot(
				db, artifacts, journal,
			)
			if err != nil {
				return false, err
			}
		}
		if err := validateMySQLOptionTerminalJournalSnapshot(
			db, artifacts, journal,
		); err != nil {
			return false, err
		}
		return false, nil
	}
	if !journal.hasSnapshot() {
		return false, fmt.Errorf(
			"options migration artifact collision: incomplete completion journal",
		)
	}
	state = mysqlOptionMigrationState{
		Phase:              journal.Phase,
		SourceSchemaSHA256: journal.SourceSchemaSHA256,
	}
	if err := validateTerminalMySQLOptionMigration(
		db, artifacts, state, journal.TerminalSchemaSHA256,
		journal.ActiveRowsSHA256, journal.RetainedRowsSHA256,
		journal.RetainedCanonicalRowsSHA256,
	); err != nil {
		return false, err
	}
	if err := clearMySQLOptionTerminalJournal(
		db, artifacts, state, journal,
	); err != nil {
		return false, err
	}
	return true, nil
}

func mysqlOptionTerminalJournalRecoveryBlocked(err error) error {
	blocked := errors.Join(
		errMySQLOptionFinalizationRecoveryBlocked,
		errMySQLOptionTerminalJournalRecoveryBlocked,
	)
	switch {
	case errors.Is(err, errMySQLMetadataVisibilityInspection):
		return errors.Join(blocked, errMySQLMetadataVisibilityInspection)
	case errors.Is(err, errMySQLGlobalMetadataVisibilityRequired):
		return errors.Join(blocked, errMySQLGlobalMetadataVisibilityRequired)
	case errors.Is(err, errMySQLOptionTerminalJournalInspection):
		return errors.Join(blocked, errMySQLOptionTerminalJournalInspection)
	case err != nil && err.Error() == "MySQL options schema changed during migration":
		return errors.Join(
			blocked,
			errors.New("MySQL options schema changed during migration"),
		)
	default:
		return blocked
	}
}

func createMySQLOptionTerminalJournal(
	db *gorm.DB,
	journal mysqlOptionTerminalJournal,
) error {
	if !validMySQLOptionTerminalJournal(journal) || journal.hasSnapshot() {
		return fmt.Errorf("create options completion journal: invalid record")
	}
	if _, exists, err := inspectMySQLOptionTerminalJournal(db); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("options migration artifact collision: completion journal")
	}
	if err := db.Exec(
		"CREATE TABLE " + quoteMySQLIdent(optionTerminalJournalTable) + " (" +
			"`singleton` TINYINT UNSIGNED NOT NULL COMMENT '" + optionTerminalJournalOwner + "'," +
			"`active_rows_sha256` CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT '" +
			optionTerminalActiveRows + "'," +
			"`retained_rows_sha256` CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT '" +
			optionTerminalRetainedRows + "'," +
			"`retained_canonical_rows_sha256` CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT '" +
			optionTerminalCanonicalRows + "'," +
			" PRIMARY KEY (`singleton`)" +
			") ENGINE=InnoDB DEFAULT CHARACTER SET ascii COLLATE ascii_bin COMMENT='" +
			journal.value() + "'",
	).Error; err != nil {
		return fmt.Errorf("create options completion journal")
	}
	return nil
}

func inspectMySQLOptionTerminalJournal(
	db *gorm.DB,
) (mysqlOptionTerminalJournal, bool, error) {
	var tables []struct {
		TableType string `gorm:"column:table_type"`
		Engine    string `gorm:"column:engine"`
		Collation string `gorm:"column:table_collation"`
		Comment   string `gorm:"column:table_comment"`
		Options   string `gorm:"column:create_options"`
	}
	if err := db.Raw(`
SELECT table_type, engine, table_collation, table_comment, create_options
FROM information_schema.tables
WHERE table_schema = DATABASE() AND table_name = ?`,
		optionTerminalJournalTable).Scan(&tables).Error; err != nil {
		return mysqlOptionTerminalJournal{}, false, errMySQLOptionTerminalJournalInspection
	}
	if len(tables) == 0 {
		return mysqlOptionTerminalJournal{}, false, nil
	}
	if len(tables) != 1 ||
		!strings.EqualFold(tables[0].TableType, "BASE TABLE") ||
		!strings.EqualFold(tables[0].Engine, "InnoDB") ||
		!strings.EqualFold(tables[0].Collation, "ascii_bin") ||
		tables[0].Options != "" {
		return mysqlOptionTerminalJournal{}, false,
			fmt.Errorf("options migration artifact collision: completion journal")
	}
	journal, ok := parseMySQLOptionTerminalJournal(tables[0].Comment)
	if !ok {
		return mysqlOptionTerminalJournal{}, false,
			fmt.Errorf("options migration artifact collision: completion journal")
	}
	if err := validateMySQLGlobalTableMetadataVisibility(db); err != nil {
		return mysqlOptionTerminalJournal{}, false, err
	}

	var columns []mysqlOptionTerminalJournalColumn
	if err := db.Raw(`
SELECT column_name, data_type, column_type, is_nullable, column_default, extra,
       column_comment, character_set_name, collation_name
FROM information_schema.columns
WHERE table_schema = DATABASE() AND table_name = ?
ORDER BY ordinal_position`, optionTerminalJournalTable).Scan(&columns).Error; err != nil {
		return mysqlOptionTerminalJournal{}, false, errMySQLOptionTerminalJournalInspection
	}
	if len(columns) != 4 ||
		columns[0].Name != "singleton" ||
		columns[0].DataType != "tinyint" ||
		(!strings.EqualFold(columns[0].ColumnType, "tinyint(3) unsigned") &&
			!strings.EqualFold(columns[0].ColumnType, "tinyint unsigned")) ||
		!strings.EqualFold(columns[0].Nullable, "NO") ||
		columns[0].DefaultValue.Valid ||
		columns[0].Extra != "" ||
		columns[0].Comment != optionTerminalJournalOwner ||
		columns[0].CharacterSet.Valid ||
		columns[0].Collation.Valid ||
		!validMySQLOptionTerminalDigestColumn(columns[1], "active_rows_sha256",
			optionTerminalActiveRows) ||
		!validMySQLOptionTerminalDigestColumn(columns[2], "retained_rows_sha256",
			optionTerminalRetainedRows) ||
		!validMySQLOptionTerminalDigestColumn(columns[3], "retained_canonical_rows_sha256",
			optionTerminalCanonicalRows) {
		return mysqlOptionTerminalJournal{}, false,
			fmt.Errorf("options migration artifact collision: completion journal")
	}

	var indexes []struct {
		Name       string `gorm:"column:index_name"`
		NonUnique  int    `gorm:"column:non_unique"`
		Sequence   int    `gorm:"column:seq_in_index"`
		ColumnName string `gorm:"column:column_name"`
		IndexType  string `gorm:"column:index_type"`
	}
	if err := db.Raw(`
SELECT index_name, non_unique, seq_in_index, column_name, index_type
FROM information_schema.statistics
WHERE table_schema = DATABASE() AND table_name = ?
ORDER BY index_name, seq_in_index`, optionTerminalJournalTable).Scan(&indexes).Error; err != nil {
		return mysqlOptionTerminalJournal{}, false, errMySQLOptionTerminalJournalInspection
	}
	if len(indexes) != 1 ||
		indexes[0].Name != "PRIMARY" ||
		indexes[0].NonUnique != 0 ||
		indexes[0].Sequence != 1 ||
		indexes[0].ColumnName != "singleton" ||
		!strings.EqualFold(indexes[0].IndexType, "BTREE") {
		return mysqlOptionTerminalJournal{}, false,
			fmt.Errorf("options migration artifact collision: completion journal")
	}

	var relations int64
	if err := db.Raw(`
SELECT
  (SELECT count(*)
   FROM information_schema.triggers
   WHERE trigger_schema = DATABASE() AND event_object_table = ?)
  +
  (SELECT count(*)
   FROM information_schema.key_column_usage
   WHERE
     (table_schema = DATABASE() AND table_name = ? AND referenced_table_name IS NOT NULL)
     OR
     (referenced_table_schema = DATABASE() AND referenced_table_name = ?))`,
		optionTerminalJournalTable,
		optionTerminalJournalTable,
		optionTerminalJournalTable,
	).Scan(&relations).Error; err != nil {
		return mysqlOptionTerminalJournal{}, false, errMySQLOptionTerminalJournalInspection
	}
	if relations != 0 {
		return mysqlOptionTerminalJournal{}, false,
			fmt.Errorf("options migration artifact collision: completion journal")
	}

	var rows int64
	if err := db.Table(optionTerminalJournalTable).Count(&rows).Error; err != nil {
		return mysqlOptionTerminalJournal{}, false, errMySQLOptionTerminalJournalInspection
	}
	if rows == 0 {
		return journal, true, nil
	}
	if rows != 1 {
		return mysqlOptionTerminalJournal{}, false,
			fmt.Errorf("options migration artifact collision: completion journal")
	}
	var snapshots []mysqlOptionTerminalJournalSnapshot
	if err := db.Table(optionTerminalJournalTable).
		Select(
			"singleton",
			"active_rows_sha256",
			"retained_rows_sha256",
			"retained_canonical_rows_sha256",
		).
		Find(&snapshots).Error; err != nil {
		return mysqlOptionTerminalJournal{}, false, errMySQLOptionTerminalJournalInspection
	}
	if len(snapshots) != 1 ||
		snapshots[0].Singleton != 1 ||
		!validOptionMigrationSchemaDigest(snapshots[0].ActiveRowsSHA256) ||
		!validOptionMigrationSchemaDigest(snapshots[0].RetainedRowsSHA256) ||
		!validOptionMigrationSchemaDigest(snapshots[0].RetainedCanonicalRowsSHA256) {
		return mysqlOptionTerminalJournal{}, false,
			fmt.Errorf("options migration artifact collision: completion journal")
	}
	journal.ActiveRowsSHA256 = snapshots[0].ActiveRowsSHA256
	journal.RetainedRowsSHA256 = snapshots[0].RetainedRowsSHA256
	journal.RetainedCanonicalRowsSHA256 = snapshots[0].RetainedCanonicalRowsSHA256
	return journal, true, nil
}

func validMySQLOptionTerminalDigestColumn(
	column mysqlOptionTerminalJournalColumn,
	name string,
	comment string,
) bool {
	return column.Name == name &&
		column.DataType == "char" &&
		strings.EqualFold(column.ColumnType, "char(64)") &&
		strings.EqualFold(column.Nullable, "NO") &&
		!column.DefaultValue.Valid &&
		column.Extra == "" &&
		column.Comment == comment &&
		column.CharacterSet.Valid &&
		strings.EqualFold(column.CharacterSet.String, "ascii") &&
		column.Collation.Valid &&
		strings.EqualFold(column.Collation.String, "ascii_bin")
}

func parseMySQLOptionTerminalJournal(value string) (mysqlOptionTerminalJournal, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 5 || parts[0] != optionTerminalJournalMark {
		return mysqlOptionTerminalJournal{}, false
	}
	journal := mysqlOptionTerminalJournal{
		ID:                   parts[1],
		Phase:                parts[2],
		SourceSchemaSHA256:   parts[3],
		TerminalSchemaSHA256: parts[4],
	}
	return journal, validMySQLOptionTerminalJournal(journal)
}

func validMySQLOptionTerminalJournal(journal mysqlOptionTerminalJournal) bool {
	if !isMySQLOptionMigrationArtifactID(journal.ID) ||
		journal.Phase != optionMigrationBridgesGone ||
		!validOptionMigrationSchemaDigest(journal.SourceSchemaSHA256) ||
		!validOptionMigrationSchemaDigest(journal.TerminalSchemaSHA256) {
		return false
	}
	if journal.ActiveRowsSHA256 == "" &&
		journal.RetainedRowsSHA256 == "" &&
		journal.RetainedCanonicalRowsSHA256 == "" {
		return true
	}
	return validOptionMigrationSchemaDigest(journal.ActiveRowsSHA256) &&
		validOptionMigrationSchemaDigest(journal.RetainedRowsSHA256) &&
		validOptionMigrationSchemaDigest(journal.RetainedCanonicalRowsSHA256)
}

func persistMySQLOptionTerminalJournalSnapshot(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	expected mysqlOptionTerminalJournal,
) (mysqlOptionTerminalJournal, error) {
	journal, exists, err := inspectMySQLOptionTerminalJournal(db)
	if err != nil {
		return mysqlOptionTerminalJournal{}, err
	}
	if !exists || journal != expected || journal.hasSnapshot() ||
		journal.ID != artifacts.ID ||
		!hmac.Equal(
			[]byte(journal.SourceSchemaSHA256),
			[]byte(artifacts.SourceSchemaSHA256),
		) {
		return mysqlOptionTerminalJournal{},
			fmt.Errorf("options migration artifact collision: completion journal")
	}
	terminalDigest, err := mysqlOptionTerminalSchemaDigest(db, "options")
	if err != nil ||
		!hmac.Equal(
			[]byte(terminalDigest),
			[]byte(journal.TerminalSchemaSHA256),
		) {
		return mysqlOptionTerminalJournal{},
			fmt.Errorf("MySQL options schema changed during migration")
	}
	rowDigests, err := mysqlOptionTerminalRowDigests(db, artifacts)
	if err != nil ||
		!hmac.Equal(
			[]byte(rowDigests.ActiveSemantic),
			[]byte(rowDigests.RetainedCanonicalSemantic),
		) {
		return mysqlOptionTerminalJournal{},
			fmt.Errorf("MySQL options row data changed during migration")
	}
	result := db.Exec(
		"INSERT INTO "+quoteMySQLIdent(optionTerminalJournalTable)+
			" (`singleton`, `active_rows_sha256`, `retained_rows_sha256`, "+
			"`retained_canonical_rows_sha256`) VALUES (1, ?, ?, ?)",
		rowDigests.ActiveRaw, rowDigests.RetainedRaw,
		rowDigests.RetainedCanonicalSemantic,
	)
	if result.Error != nil || result.RowsAffected != 1 {
		return mysqlOptionTerminalJournal{},
			fmt.Errorf("persist options completion journal snapshot")
	}
	journal.ActiveRowsSHA256 = rowDigests.ActiveRaw
	journal.RetainedRowsSHA256 = rowDigests.RetainedRaw
	journal.RetainedCanonicalRowsSHA256 = rowDigests.RetainedCanonicalSemantic
	return journal, nil
}

func validateMySQLOptionTerminalJournalSnapshot(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	expected mysqlOptionTerminalJournal,
) error {
	journal, exists, err := inspectMySQLOptionTerminalJournal(db)
	if err != nil {
		return err
	}
	if !exists || !journal.hasSnapshot() || journal != expected ||
		journal.ID != artifacts.ID ||
		!hmac.Equal(
			[]byte(journal.SourceSchemaSHA256),
			[]byte(artifacts.SourceSchemaSHA256),
		) {
		return fmt.Errorf("options migration artifact collision: completion journal")
	}
	terminalDigest, err := mysqlOptionTerminalSchemaDigest(db, "options")
	if err != nil ||
		!hmac.Equal(
			[]byte(terminalDigest),
			[]byte(journal.TerminalSchemaSHA256),
		) {
		return fmt.Errorf("MySQL options schema changed during migration")
	}
	return validateMySQLOptionTerminalRows(
		db, artifacts, journal.ActiveRowsSHA256, journal.RetainedRowsSHA256,
		journal.RetainedCanonicalRowsSHA256,
	)
}

func validateMarkedMySQLOptionTerminalJournal(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	state mysqlOptionMigrationState,
	expected mysqlOptionTerminalJournal,
) error {
	journal, exists, err := inspectMySQLOptionTerminalJournal(db)
	if err != nil {
		return err
	}
	if !exists || journal != expected ||
		journal.ID != artifacts.ID ||
		state.Phase != optionMigrationBridgesGone ||
		!hmac.Equal(
			[]byte(journal.SourceSchemaSHA256),
			[]byte(state.SourceSchemaSHA256),
		) {
		return fmt.Errorf("options migration artifact collision: completion journal")
	}
	if err := validateMySQLOptionFinalizationStateForBridgePhase(
		db, artifacts, state, optionMigrationBridgesGone,
	); err != nil {
		return err
	}
	return validateMySQLOptionTerminalJournalSnapshot(db, artifacts, expected)
}

func clearMySQLOptionTerminalJournal(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	state mysqlOptionMigrationState,
	expected mysqlOptionTerminalJournal,
) error {
	journal, exists, err := inspectMySQLOptionTerminalJournal(db)
	if err != nil {
		return err
	}
	if !exists || journal != expected {
		return fmt.Errorf("options migration artifact collision: completion journal")
	}
	if err := validateTerminalMySQLOptionMigration(
		db, artifacts, state, expected.TerminalSchemaSHA256,
		expected.ActiveRowsSHA256, expected.RetainedRowsSHA256,
		expected.RetainedCanonicalRowsSHA256,
	); err != nil {
		return err
	}
	if err := db.Exec(
		"DROP TABLE " + quoteMySQLIdent(optionTerminalJournalTable),
	).Error; err != nil {
		return fmt.Errorf("drop options completion journal")
	}
	if err := lockMySQLOptionTerminalTables(db, artifacts.BackupTable, false); err != nil {
		return err
	}
	if err := validateTerminalMySQLOptionMigration(
		db, artifacts, state, expected.TerminalSchemaSHA256,
		"", "", "",
	); err != nil {
		return err
	}
	if _, exists, err := inspectMySQLOptionTerminalJournal(db); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("options migration artifact collision: completion journal")
	}
	return nil
}

func lockMySQLOptionFinalizationTables(
	db *gorm.DB,
	retained string,
	includeJournal bool,
) error {
	return lockMySQLOptionTerminalTables(db, retained, includeJournal)
}

func lockMySQLOptionTerminalTables(
	db *gorm.DB,
	retained string,
	includeJournal bool,
) error {
	if !optionSafeIdent(retained) {
		return fmt.Errorf("lock options finalization tables: unsafe retained table name")
	}
	statement := "LOCK TABLES `options` WRITE, " + quoteMySQLIdent(retained) +
		" WRITE, " + quoteMySQLIdent(retained) + " AS source_row READ"
	if includeJournal {
		statement += ", " + quoteMySQLIdent(optionTerminalJournalTable) + " WRITE"
	}
	if err := db.Exec(statement).Error; err != nil {
		return fmt.Errorf("lock options finalization tables: %w", err)
	}
	return nil
}

func restoreMySQLOptionFinalizationState(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	state mysqlOptionMigrationState,
	retainedTriggers []mysqlOptionTrigger,
	point mysqlOptionFinalizationRestorePoint,
) error {
	if err := validateMySQLOptionFinalizationRestoreState(
		db, artifacts, state, retainedTriggers, point,
	); err != nil {
		return err
	}
	operation, ok := nextMySQLOptionRestoreOperation(state.Phase, point.BridgePhase)
	if !ok {
		return errMySQLOptionFinalizationRecoveryBlocked
	}
	intent := mysqlOptionRestoreIntent{
		TargetPhase: state.Phase,
		Operation:   operation,
	}
	intentState := state
	intentState.Phase = intent.phase()
	if point.MarkersDropped {
		if err := db.Exec(
			"ALTER TABLE `options` ADD COLUMN " + quoteMySQLIdent(optionMigrationMarkerCol) +
				" TINYINT UNSIGNED NOT NULL DEFAULT 1 COMMENT '" + artifacts.Marker +
				"', ADD COLUMN " + quoteMySQLIdent(optionMigrationStateCol) +
				fmt.Sprintf(" VARCHAR(%d) NOT NULL DEFAULT '", optionMigrationStateSize) +
				intentState.value() + "' COMMENT '" + optionMigrationStateMarker + "'",
		).Error; err != nil {
			return errMySQLOptionFinalizationRecoveryBlocked
		}
	} else {
		if err := setMySQLOptionMigrationPhase(db, state, intent.phase()); err != nil {
			return errMySQLOptionFinalizationRecoveryBlocked
		}
	}
	return resumeMySQLOptionFinalizationRestore(db, artifacts, state, intent)
}

func resumeMySQLOptionFinalizationRestore(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	targetState mysqlOptionMigrationState,
	intent mysqlOptionRestoreIntent,
) error {
	if !validMySQLOptionMigrationPhase(targetState.Phase) ||
		intent.TargetPhase != targetState.Phase {
		return errMySQLOptionFinalizationRecoveryBlocked
	}
	for {
		bridgePhase, retainedTriggers, err := inspectMySQLOptionRestoreIntentState(
			db, artifacts, targetState, intent,
		)
		if err != nil {
			return err
		}
		preBridgePhase, postBridgePhase, valid := mysqlOptionRestoreOperationBridgePhases(intent)
		if !valid || bridgePhase != preBridgePhase && bridgePhase != postBridgePhase {
			return errMySQLOptionFinalizationRecoveryBlocked
		}

		if intent.Operation == optionRestoreClear {
			if err := setMySQLOptionMigrationPhase(
				db, targetState, targetState.Phase,
			); err != nil {
				return errMySQLOptionFinalizationRecoveryBlocked
			}
			if err := lockMySQLOptionFinalizationTables(
				db, artifacts.BackupTable, false,
			); err != nil {
				return mysqlOptionFinalizationRestoreBlocked(err)
			}
			if _, err := readAndValidateMySQLOptionFinalizationState(
				db, artifacts, targetState.Phase, targetState.Phase,
			); err != nil {
				return mysqlOptionFinalizationRestoreBlocked(err)
			}
			return nil
		}

		if bridgePhase == preBridgePhase {
			var triggerName string
			switch intent.Operation {
			case optionRestoreCreateDelete:
				triggerName = artifacts.DeleteTrigger
			case optionRestoreCreateUpdate:
				triggerName = artifacts.UpdateTrigger
			case optionRestoreCreateInsert:
				triggerName = artifacts.InsertTrigger
			}
			var trigger mysqlOptionTrigger
			for _, candidate := range retainedTriggers {
				if candidate.Name == triggerName {
					trigger = candidate
					break
				}
			}
			if trigger.Name == "" {
				return errMySQLOptionFinalizationRecoveryBlocked
			}
			if err := db.Exec(
				"CREATE TRIGGER " + quoteMySQLIdent(trigger.Name) + " " + trigger.Timing +
					" " + trigger.Event + " ON " + quoteMySQLIdent(artifacts.BackupTable) +
					" FOR EACH ROW " + trigger.Action,
			).Error; err != nil {
				return errMySQLOptionFinalizationRecoveryBlocked
			}
			bridgePhase, _, err = inspectMySQLOptionRestoreIntentState(
				db, artifacts, targetState, intent,
			)
			if err != nil {
				return err
			}
			if bridgePhase != postBridgePhase {
				return errMySQLOptionFinalizationRecoveryBlocked
			}
		}

		nextOperation, ok := nextMySQLOptionRestoreOperation(
			targetState.Phase, postBridgePhase,
		)
		if !ok {
			return errMySQLOptionFinalizationRecoveryBlocked
		}
		intent = mysqlOptionRestoreIntent{
			TargetPhase: targetState.Phase,
			Operation:   nextOperation,
		}
		if err := setMySQLOptionMigrationPhase(
			db, targetState, intent.phase(),
		); err != nil {
			return errMySQLOptionFinalizationRecoveryBlocked
		}
	}
}

func resumeMySQLOptionFinalizationForward(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	targetState mysqlOptionMigrationState,
	intent mysqlOptionForwardIntent,
	journal *mysqlOptionTerminalJournal,
) error {
	includeJournal := journal != nil
	if !validMySQLOptionMigrationPhase(targetState.Phase) ||
		intent.TargetPhase != targetState.Phase {
		return errMySQLOptionFinalizationRecoveryBlocked
	}
	preBridgePhase, postBridgePhase, triggerKind, valid :=
		mysqlOptionForwardOperationBridgePhases(intent)
	if !valid {
		return errMySQLOptionFinalizationRecoveryBlocked
	}
	bridgePhase, err := inspectMySQLOptionForwardIntentState(
		db, artifacts, targetState, intent, includeJournal,
	)
	if err != nil {
		return mysqlOptionFinalizationRestoreBlocked(err)
	}
	if journal != nil {
		if err := validateMySQLOptionTerminalJournalSnapshot(
			db, artifacts, *journal,
		); err != nil {
			return err
		}
	}
	if bridgePhase != preBridgePhase && bridgePhase != postBridgePhase {
		return errMySQLOptionFinalizationRecoveryBlocked
	}
	if bridgePhase == preBridgePhase {
		trigger := artifacts.InsertTrigger
		switch triggerKind {
		case "update":
			trigger = artifacts.UpdateTrigger
		case "delete":
			trigger = artifacts.DeleteTrigger
		}
		if err := db.Exec("DROP TRIGGER " + quoteMySQLIdent(trigger)).Error; err != nil {
			return errMySQLOptionFinalizationRecoveryBlocked
		}
		bridgePhase, err = inspectMySQLOptionForwardIntentState(
			db, artifacts, targetState, intent, includeJournal,
		)
		if err != nil {
			return mysqlOptionFinalizationRestoreBlocked(err)
		}
		if journal != nil {
			if err := validateMySQLOptionTerminalJournalSnapshot(
				db, artifacts, *journal,
			); err != nil {
				return err
			}
		}
		if bridgePhase != postBridgePhase {
			return errMySQLOptionFinalizationRecoveryBlocked
		}
	}
	if err := setMySQLOptionMigrationPhase(
		db, targetState, targetState.Phase,
	); err != nil {
		return errMySQLOptionFinalizationRecoveryBlocked
	}
	if err := lockMySQLOptionFinalizationTables(
		db, artifacts.BackupTable, includeJournal,
	); err != nil {
		return mysqlOptionFinalizationRestoreBlocked(err)
	}
	if _, err := readAndValidateMySQLOptionFinalizationState(
		db, artifacts, targetState.Phase, targetState.Phase,
	); err != nil {
		return mysqlOptionFinalizationRestoreBlocked(err)
	}
	if journal != nil {
		return validateMySQLOptionTerminalJournalSnapshot(
			db, artifacts, *journal,
		)
	}
	return nil
}

func inspectMySQLOptionForwardIntentState(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	targetState mysqlOptionMigrationState,
	intent mysqlOptionForwardIntent,
	includeJournal bool,
) (string, error) {
	if err := lockMySQLOptionFinalizationTables(
		db, artifacts.BackupTable, includeJournal,
	); err != nil {
		return "", err
	}
	state, hasState, err := inspectMySQLOptionMigrationState(db, "options")
	if err != nil {
		return "", err
	}
	if !hasState || state.Phase != intent.phase() ||
		!hmac.Equal([]byte(state.SourceSchemaSHA256), []byte(targetState.SourceSchemaSHA256)) ||
		!hmac.Equal([]byte(state.ActiveSchemaSHA256), []byte(targetState.ActiveSchemaSHA256)) {
		return "", fmt.Errorf("options migration artifact collision: invalid forward intent")
	}
	bridgePhase, _, err := inspectMySQLOptionFinalizationBridgePhase(
		db, artifacts, state, targetState.Phase,
	)
	if err != nil {
		return "", err
	}
	return bridgePhase, nil
}

func inspectMySQLOptionRestoreIntentState(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	targetState mysqlOptionMigrationState,
	intent mysqlOptionRestoreIntent,
) (string, []mysqlOptionTrigger, error) {
	if err := lockMySQLOptionFinalizationTables(
		db, artifacts.BackupTable, false,
	); err != nil {
		return "", nil, mysqlOptionFinalizationRestoreBlocked(err)
	}
	state, hasState, err := inspectMySQLOptionMigrationState(db, "options")
	if err != nil {
		return "", nil, mysqlOptionFinalizationRestoreBlocked(err)
	}
	if !hasState || state.Phase != intent.phase() ||
		!hmac.Equal([]byte(state.SourceSchemaSHA256), []byte(targetState.SourceSchemaSHA256)) ||
		!hmac.Equal([]byte(state.ActiveSchemaSHA256), []byte(targetState.ActiveSchemaSHA256)) {
		return "", nil, errMySQLOptionFinalizationRecoveryBlocked
	}
	bridgePhase, retainedTriggers, err := inspectMySQLOptionFinalizationBridgePhase(
		db, artifacts, state, targetState.Phase,
	)
	if err != nil {
		return "", nil, mysqlOptionFinalizationRestoreBlocked(err)
	}
	return bridgePhase, retainedTriggers, nil
}

func mysqlOptionFinalizationRestoreBlocked(err error) error {
	switch {
	case errors.Is(err, errMySQLMetadataVisibilityInspection):
		return errors.Join(
			errMySQLOptionFinalizationRecoveryBlocked,
			errMySQLMetadataVisibilityInspection,
		)
	case errors.Is(err, errMySQLGlobalMetadataVisibilityRequired):
		return errors.Join(
			errMySQLOptionFinalizationRecoveryBlocked,
			errMySQLGlobalMetadataVisibilityRequired,
		)
	default:
		return errMySQLOptionFinalizationRecoveryBlocked
	}
}

func validateMySQLOptionFinalizationRestoreState(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	initialState mysqlOptionMigrationState,
	retainedTriggers []mysqlOptionTrigger,
	point mysqlOptionFinalizationRestorePoint,
) error {
	if err := lockMySQLOptionFinalizationTables(
		db, artifacts.BackupTable, false,
	); err != nil {
		return mysqlOptionFinalizationRestoreBlocked(err)
	}
	if err := validateMySQLGlobalTableMetadataVisibility(db); err != nil {
		return mysqlOptionFinalizationRestoreBlocked(err)
	}

	if point.StatePhase == "" || point.BridgePhase == "" {
		return mysqlOptionFinalizationRestoreBlocked(fmt.Errorf("invalid recovery point"))
	}
	state, hasState, err := inspectMySQLOptionMigrationState(db, "options")
	if err != nil {
		return mysqlOptionFinalizationRestoreBlocked(err)
	}
	owned, err := hasOwnedMySQLOptionMigrationMarker(db, "options", artifacts.Marker)
	if err != nil {
		return mysqlOptionFinalizationRestoreBlocked(err)
	}
	if point.MarkersDropped {
		if hasState || owned {
			return mysqlOptionFinalizationRestoreBlocked(fmt.Errorf("invalid marker cleanup state"))
		}
		if !validOptionMigrationSchemaDigest(point.TerminalSchemaSHA256) {
			return mysqlOptionFinalizationRestoreBlocked(fmt.Errorf("invalid terminal recovery schema"))
		}
		activeDigest, err := mysqlOptionCurrentSchemaDigest(db, "options")
		if err != nil ||
			!hmac.Equal([]byte(activeDigest), []byte(point.TerminalSchemaSHA256)) {
			return mysqlOptionFinalizationRestoreBlocked(fmt.Errorf("invalid terminal recovery schema"))
		}
		primary, err := mysqlOptionKeyIsSolePrimary(db, "options")
		if err != nil || !primary {
			return mysqlOptionFinalizationRestoreBlocked(fmt.Errorf("invalid recovery primary key"))
		}
	} else {
		if !hasState || !owned || state.Phase != point.StatePhase ||
			!hmac.Equal([]byte(state.SourceSchemaSHA256), []byte(initialState.SourceSchemaSHA256)) ||
			!hmac.Equal([]byte(state.ActiveSchemaSHA256), []byte(initialState.ActiveSchemaSHA256)) {
			return mysqlOptionFinalizationRestoreBlocked(fmt.Errorf("invalid recovery state"))
		}
		if err := validateMySQLOptionActiveSchema(
			db, "options", artifacts, state,
		); err != nil {
			return mysqlOptionFinalizationRestoreBlocked(err)
		}
	}

	retainedDDL, err := showCreateMySQLTable(db, artifacts.BackupTable)
	if err != nil {
		return mysqlOptionFinalizationRestoreBlocked(err)
	}
	normalizedRetained, ok := replaceMySQLCreateTableName(
		retainedDDL, artifacts.BackupTable, "options",
	)
	if !ok {
		return mysqlOptionFinalizationRestoreBlocked(fmt.Errorf("invalid retained recovery schema"))
	}
	retainedDigest := sha256.Sum256([]byte(normalizedRetained))
	if !hmac.Equal(
		[]byte(hex.EncodeToString(retainedDigest[:])),
		[]byte(initialState.SourceSchemaSHA256),
	) {
		return mysqlOptionFinalizationRestoreBlocked(fmt.Errorf("invalid retained recovery schema"))
	}
	if err := validateMySQLOptionFinalizationCatalog(
		db, artifacts.BackupTable,
	); err != nil {
		return mysqlOptionFinalizationRestoreBlocked(err)
	}

	targetNames, validTargetPhase := mysqlOptionMigrationBridgeNamesForPhase(
		artifacts, initialState.Phase,
	)
	expectedNames, validBridgePhase := mysqlOptionMigrationBridgeNamesForPhase(
		artifacts, point.BridgePhase,
	)
	if !validTargetPhase || !validBridgePhase {
		return mysqlOptionFinalizationRestoreBlocked(fmt.Errorf("invalid recovery bridge phase"))
	}
	targetSet := make(map[string]struct{}, len(targetNames))
	for _, name := range targetNames {
		targetSet[name] = struct{}{}
	}
	retainedByName := make(map[string]mysqlOptionTrigger, len(retainedTriggers))
	for _, trigger := range retainedTriggers {
		if _, expected := targetSet[trigger.Name]; !expected {
			return mysqlOptionFinalizationRestoreBlocked(fmt.Errorf("invalid retained recovery bridge"))
		}
		if _, duplicate := retainedByName[trigger.Name]; duplicate {
			return mysqlOptionFinalizationRestoreBlocked(fmt.Errorf("duplicate retained recovery bridge"))
		}
		retainedByName[trigger.Name] = trigger
	}
	if len(retainedByName) != len(targetSet) {
		return mysqlOptionFinalizationRestoreBlocked(fmt.Errorf("incomplete retained recovery bridge"))
	}

	var triggerCount int64
	if err := db.Raw(`
SELECT count(*)
FROM information_schema.triggers
WHERE trigger_schema = DATABASE() AND event_object_table = ?`,
		artifacts.BackupTable).Scan(&triggerCount).Error; err != nil {
		return mysqlOptionFinalizationRestoreBlocked(err)
	}
	triggers, err := inspectMySQLOptionMigrationTriggerCandidates(db, artifacts)
	if err != nil {
		return mysqlOptionFinalizationRestoreBlocked(err)
	}
	if triggerCount != int64(len(expectedNames)) ||
		len(triggers) != len(expectedNames) {
		return mysqlOptionFinalizationRestoreBlocked(fmt.Errorf("invalid recovery bridge set"))
	}
	expectedSet := make(map[string]struct{}, len(expectedNames))
	for _, name := range expectedNames {
		expectedSet[name] = struct{}{}
	}
	for _, current := range triggers {
		expected, ok := retainedByName[current.Name]
		if _, present := expectedSet[current.Name]; !present || !ok ||
			current.TableName != expected.TableName ||
			!strings.EqualFold(current.Event, expected.Event) ||
			!strings.EqualFold(current.Timing, expected.Timing) ||
			normalizeMySQLTriggerAction(current.Action) !=
				normalizeMySQLTriggerAction(expected.Action) ||
			current.TableName != artifacts.BackupTable {
			return mysqlOptionFinalizationRestoreBlocked(fmt.Errorf("invalid recovery bridge definition"))
		}
	}
	return nil
}

func readDedupedOptionRows(db *gorm.DB, digestKey []byte) ([]optionMigrationRow, []Option, error) {
	var sourceRowCount int64
	if err := db.Table("options").Count(&sourceRowCount).Error; err != nil {
		return nil, nil, fmt.Errorf("count options rows: table=options")
	}
	if sourceRowCount > optionMigrationMaxRows {
		return nil, nil, fmt.Errorf(
			"read options rows: options row limit exceeded: table=options rows=%d maximum=%d",
			sourceRowCount, optionMigrationMaxRows,
		)
	}
	rows, err := readOptionMigrationRows(db, sourceRowCount)
	if err != nil {
		return nil, nil, fmt.Errorf("read options rows: %w", err)
	}
	deduped, err := dedupeOptionRows(rows, digestKey)
	if err != nil {
		return nil, nil, err
	}
	return rows, deduped, nil
}

func newMySQLOptionMigrationArtifacts(entropy io.Reader) (mysqlOptionMigrationArtifacts, error) {
	random := make([]byte, optionArtifactIDSize)
	if _, err := io.ReadFull(entropy, random); err != nil {
		return mysqlOptionMigrationArtifacts{}, fmt.Errorf("generate options migration artifact id: %w", err)
	}
	id := hex.EncodeToString(random)
	return mysqlOptionMigrationArtifacts{
		ID:            id,
		Marker:        optionMigrationMarker + "/" + id,
		TmpTable:      optionMySQLTmpPrefix + id,
		BackupTable:   optionLegacyTablePrefix + id,
		InsertTrigger: optionInsertTriggerPrefix + id,
		UpdateTrigger: optionUpdateTriggerPrefix + id,
		DeleteTrigger: optionDeleteTriggerPrefix + id,
	}, nil
}

func newAvailableMySQLOptionMigrationArtifacts(
	db *gorm.DB,
	entropy io.Reader,
) (mysqlOptionMigrationArtifacts, error) {
	for attempt := 0; attempt < optionArtifactIDAttempts; attempt++ {
		artifacts, err := newMySQLOptionMigrationArtifacts(entropy)
		if err != nil {
			return mysqlOptionMigrationArtifacts{}, err
		}
		available, err := mysqlOptionMigrationArtifactsAvailable(db, artifacts)
		if err != nil {
			return mysqlOptionMigrationArtifacts{}, err
		}
		if available {
			return artifacts, nil
		}
	}
	return mysqlOptionMigrationArtifacts{}, fmt.Errorf(
		"generate options migration artifact id: no unused id after %d attempts",
		optionArtifactIDAttempts,
	)
}

func mysqlOptionMigrationArtifactsAvailable(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
) (bool, error) {
	var collisions int64
	if err := db.Raw(`
SELECT
  (SELECT count(*)
   FROM information_schema.tables
   WHERE table_schema = DATABASE() AND table_name IN (?, ?))
  +
  (SELECT count(*)
   FROM information_schema.triggers
   WHERE trigger_schema = DATABASE() AND trigger_name IN (?, ?, ?))`,
		artifacts.TmpTable, artifacts.BackupTable,
		artifacts.InsertTrigger, artifacts.UpdateTrigger, artifacts.DeleteTrigger,
	).Scan(&collisions).Error; err != nil {
		return false, fmt.Errorf("inspect options migration artifact id availability: %w", err)
	}
	return collisions == 0, nil
}

func legacyMySQLOptionMigrationArtifacts() mysqlOptionMigrationArtifacts {
	return mysqlOptionMigrationArtifacts{
		Marker:        optionMigrationMarker,
		TmpTable:      optionPrimaryKeyTmpTable,
		InsertTrigger: optionInsertTrigger,
		UpdateTrigger: optionUpdateTrigger,
		DeleteTrigger: optionDeleteTrigger,
	}
}

func (artifacts mysqlOptionMigrationArtifacts) triggerNames() []string {
	return []string{artifacts.InsertTrigger, artifacts.UpdateTrigger, artifacts.DeleteTrigger}
}

func cleanupMySQLOptionMigrationArtifacts(db *gorm.DB) error {
	hasArtifacts, err := hasMySQLOptionMigrationArtifacts(db)
	if err != nil {
		return err
	}
	if !hasArtifacts {
		return nil
	}
	if err := validateMySQLGlobalTableMetadataVisibility(db); err != nil {
		return err
	}
	var markers []struct {
		Table  string `gorm:"column:table_name"`
		Marker string `gorm:"column:column_comment"`
	}
	if err := db.Raw(`
SELECT table_name, column_comment
FROM information_schema.columns
WHERE table_schema = DATABASE()
  AND column_name = ?
  AND data_type = 'tinyint'
  AND column_type LIKE '%unsigned'
  AND is_nullable = 'NO'
  AND column_default = '1'
ORDER BY table_name`, optionMigrationMarkerCol).Scan(&markers).Error; err != nil {
		return fmt.Errorf("inspect MySQL migration markers: %w", err)
	}

	type cleanupPlan struct {
		markerTable string
		artifacts   mysqlOptionMigrationArtifacts
		triggers    []mysqlOptionTrigger
	}
	plans := make([]cleanupPlan, 0, len(markers))
	ownedTables := make(map[string]struct{}, len(markers))
	ownedTriggers := make(map[string]struct{}, len(markers)*3)
	for _, owned := range markers {
		artifacts, ok := parseMySQLOptionMigrationArtifacts(owned.Table, owned.Marker)
		if !ok {
			if owned.Table == "options" || isMySQLOptionMigrationTableName(owned.Table) {
				return fmt.Errorf("options migration artifact collision: marker table=%s", owned.Table)
			}
			continue
		}
		if owned.Table == "options" {
			return fmt.Errorf("options migration artifact collision: active options ownership marker")
		}
		triggers, err := inspectOwnedMySQLOptionMigrationTriggers(db, artifacts)
		if err != nil {
			return err
		}
		for _, trigger := range triggers {
			if owned.Table == artifacts.TmpTable && trigger.TableName != "options" {
				return fmt.Errorf("options migration artifact collision: temporary artifact trigger owner")
			}
			ownedTriggers[trigger.Name] = struct{}{}
		}
		ownedTables[owned.Table] = struct{}{}
		plans = append(plans, cleanupPlan{
			markerTable: owned.Table,
			artifacts:   artifacts,
			triggers:    triggers,
		})
	}

	var candidateTables []string
	if err := db.Raw(`
SELECT table_name
FROM information_schema.tables
WHERE table_schema = DATABASE()
ORDER BY table_name`).Scan(&candidateTables).Error; err != nil {
		return fmt.Errorf("inspect MySQL migration tables: %w", err)
	}
	unmarkedDynamicTables := 0
	for _, table := range candidateTables {
		if !isMySQLOptionMigrationTableName(table) {
			continue
		}
		if _, owned := ownedTables[table]; !owned {
			if id, dynamic := strings.CutPrefix(table, optionMySQLTmpPrefix); dynamic &&
				isMySQLOptionMigrationArtifactID(id) {
				unmarkedDynamicTables++
				if unmarkedDynamicTables > optionMigrationMaxOrphans {
					return fmt.Errorf(
						"options migration unmarked temporary table limit exceeded: maximum=%d",
						optionMigrationMaxOrphans,
					)
				}
				continue
			}
			return fmt.Errorf("options migration artifact collision: table=%s", table)
		}
	}

	var candidateTriggers []string
	if err := db.Raw(`
SELECT trigger_name
FROM information_schema.triggers
WHERE trigger_schema = DATABASE()
ORDER BY trigger_name`).Scan(&candidateTriggers).Error; err != nil {
		return fmt.Errorf("inspect options migration triggers: %w", err)
	}
	for _, name := range candidateTriggers {
		if !isMySQLOptionMigrationTriggerName(name) {
			continue
		}
		if _, owned := ownedTriggers[name]; !owned {
			return fmt.Errorf("options migration artifact collision: trigger=%s", name)
		}
	}

	for _, plan := range plans {
		if err := cleanupOwnedMySQLOptionMigrationArtifacts(
			db, plan.markerTable, plan.artifacts, plan.triggers,
		); err != nil {
			return err
		}
	}
	return nil
}

func hasMySQLOptionMigrationArtifacts(db *gorm.DB) (bool, error) {
	var tables []string
	if err := db.Raw(`
SELECT table_name
FROM information_schema.tables
WHERE table_schema = DATABASE()`).Scan(&tables).Error; err != nil {
		return false, fmt.Errorf("inspect MySQL migration artifacts: %w", err)
	}
	if slices.ContainsFunc(tables, isMySQLOptionMigrationTableName) {
		return true, nil
	}
	var triggers []string
	if err := db.Raw(`
SELECT trigger_name
FROM information_schema.triggers
WHERE trigger_schema = DATABASE()`).Scan(&triggers).Error; err != nil {
		return false, fmt.Errorf("inspect MySQL migration artifacts: %w", err)
	}
	return slices.ContainsFunc(triggers, isMySQLOptionMigrationTriggerName), nil
}

func hasMySQLOptionMigrationTriggers(db *gorm.DB) (bool, error) {
	var triggers []string
	if err := db.Raw(`
SELECT trigger_name
FROM information_schema.triggers
WHERE trigger_schema = DATABASE()`).Scan(&triggers).Error; err != nil {
		return false, fmt.Errorf("inspect options migration triggers: %w", err)
	}
	return slices.ContainsFunc(triggers, isMySQLOptionMigrationTriggerName), nil
}

func isMySQLOptionMigrationTableName(name string) bool {
	return name == optionPrimaryKeyTmpTable || strings.HasPrefix(name, optionMySQLTmpPrefix)
}

func isMySQLOptionMigrationTriggerName(name string) bool {
	if name == optionInsertTrigger || name == optionUpdateTrigger || name == optionDeleteTrigger {
		return true
	}
	for _, prefix := range []string{
		optionInsertTriggerPrefix,
		optionUpdateTriggerPrefix,
		optionDeleteTriggerPrefix,
	} {
		if id, ok := strings.CutPrefix(name, prefix); ok {
			return isMySQLOptionMigrationArtifactID(id)
		}
	}
	return false
}

func parseMySQLOptionMigrationArtifacts(table, marker string) (mysqlOptionMigrationArtifacts, bool) {
	if marker == optionMigrationMarker && (table == optionPrimaryKeyTmpTable || table == "options") {
		return legacyMySQLOptionMigrationArtifacts(), true
	}
	id, ok := strings.CutPrefix(marker, optionMigrationMarker+"/")
	if !ok || !isMySQLOptionMigrationArtifactID(id) {
		return mysqlOptionMigrationArtifacts{}, false
	}
	artifacts := mysqlOptionMigrationArtifacts{
		ID:            id,
		Marker:        marker,
		TmpTable:      optionMySQLTmpPrefix + id,
		InsertTrigger: optionInsertTriggerPrefix + id,
		UpdateTrigger: optionUpdateTriggerPrefix + id,
		DeleteTrigger: optionDeleteTriggerPrefix + id,
	}
	if table != artifacts.TmpTable && table != "options" {
		return mysqlOptionMigrationArtifacts{}, false
	}
	return artifacts, true
}

func isMySQLOptionMigrationArtifactID(id string) bool {
	return len(id) == optionArtifactIDSize*2 &&
		!strings.ContainsFunc(id, func(r rune) bool {
			return r < '0' || r > '9' && r < 'a' || r > 'f'
		})
}

func cleanupOwnedMySQLOptionMigrationArtifacts(
	db *gorm.DB,
	markerTable string,
	artifacts mysqlOptionMigrationArtifacts,
	triggers []mysqlOptionTrigger,
) error {
	if markerTable == "options" {
		return fmt.Errorf("options migration artifact collision: active options ownership marker")
	}
	if err := dropOwnedMySQLOptionMigrationTriggers(db, triggers, artifacts); err != nil {
		return err
	}
	if err := db.Exec("DROP TABLE " + quoteMySQLIdent(artifacts.TmpTable)).Error; err != nil {
		return fmt.Errorf("drop leftover %s: %w", artifacts.TmpTable, err)
	}
	return nil
}

func dropMySQLOptionMigrationTriggers(db *gorm.DB, artifacts mysqlOptionMigrationArtifacts) error {
	triggers, err := inspectOwnedMySQLOptionMigrationTriggers(db, artifacts)
	if err != nil {
		return err
	}
	return dropOwnedMySQLOptionMigrationTriggers(db, triggers, artifacts)
}

func inspectOwnedMySQLOptionMigrationTriggers(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
) ([]mysqlOptionTrigger, error) {
	existing, err := inspectMySQLOptionMigrationTriggerCandidates(db, artifacts)
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 {
		return nil, nil
	}
	triggerTable := existing[0].TableName
	expectedEvents := map[string]string{
		artifacts.InsertTrigger: "INSERT",
		artifacts.UpdateTrigger: "UPDATE",
		artifacts.DeleteTrigger: "DELETE",
	}
	for _, trigger := range existing {
		if trigger.TableName != triggerTable {
			return nil, fmt.Errorf("options migration artifact collision: inconsistent trigger owners")
		}
		event, known := expectedEvents[trigger.Name]
		if !known ||
			(trigger.TableName != "options" && !isOptionLegacyTable(trigger.TableName)) ||
			!strings.EqualFold(trigger.Event, event) ||
			!strings.EqualFold(trigger.Timing, "AFTER") {
			return nil, fmt.Errorf("options migration artifact collision: trigger=%s", trigger.Name)
		}
	}
	columns, err := mysqlOwnedOptionMigrationColumns(db, artifacts, triggerTable)
	if err != nil {
		return nil, err
	}
	specs := mysqlOptionMigrationTriggerSpecs(columns, artifacts)
	specByName := make(map[string]mysqlOptionTriggerSpec, len(specs))
	for _, spec := range specs {
		specByName[spec.Name] = spec
	}
	for _, trigger := range existing {
		spec, ok := specByName[trigger.Name]
		if !ok ||
			normalizeMySQLTriggerAction(trigger.Action) != normalizeMySQLTriggerAction(spec.Action) {
			return nil, fmt.Errorf("options migration artifact collision: trigger=%s", trigger.Name)
		}
	}
	return existing, nil
}

func inspectMySQLOptionMigrationTriggerCandidates(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
) ([]mysqlOptionTrigger, error) {
	var existing []mysqlOptionTrigger
	if err := db.Raw(`
SELECT TRIGGER_NAME AS trigger_name,
       EVENT_MANIPULATION AS event_manipulation,
       ACTION_TIMING AS action_timing,
       EVENT_OBJECT_TABLE AS event_object_table,
       ACTION_STATEMENT AS action_statement
FROM information_schema.triggers
WHERE trigger_schema = DATABASE() AND trigger_name IN (?, ?, ?)`,
		artifacts.InsertTrigger, artifacts.UpdateTrigger, artifacts.DeleteTrigger,
	).Scan(&existing).Error; err != nil {
		return nil, fmt.Errorf("inspect options migration triggers: %w", err)
	}
	if len(existing) == 0 {
		return nil, nil
	}
	return existing, nil
}

func mysqlOwnedOptionMigrationColumns(
	db *gorm.DB,
	artifacts mysqlOptionMigrationArtifacts,
	triggerTable string,
) ([]string, error) {
	if triggerTable == "options" {
		owned, err := hasOwnedMySQLOptionMigrationMarker(
			db, artifacts.TmpTable, artifacts.Marker,
		)
		if err != nil {
			return nil, err
		}
		if owned {
			return mysqlWritableOptionColumnsFromTable(db, artifacts.TmpTable)
		}
	}
	return mysqlWritableOptionColumnsFromTable(db, triggerTable)
}

func dropOwnedMySQLOptionMigrationTriggers(
	db *gorm.DB,
	existing []mysqlOptionTrigger,
	artifacts mysqlOptionMigrationArtifacts,
) error {
	existingSet := make(map[string]struct{}, len(existing))
	for _, trigger := range existing {
		existingSet[trigger.Name] = struct{}{}
	}
	var errs []error
	for _, name := range artifacts.triggerNames() {
		if _, ok := existingSet[name]; !ok {
			continue
		}
		if err := db.Exec("DROP TRIGGER `" + name + "`").Error; err != nil {
			errs = append(errs, fmt.Errorf("drop options migration trigger %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func hasOwnedMySQLOptionMigrationMarker(db *gorm.DB, table, marker string) (bool, error) {
	if !optionSafeIdent(table) {
		return false, fmt.Errorf("inspect MySQL migration marker: unsafe table name")
	}
	var columns int64
	if err := db.Raw(`
SELECT count(*)
FROM information_schema.columns
WHERE table_schema = DATABASE()
  AND table_name = ?
  AND column_name = ?
  AND data_type = 'tinyint'
  AND column_type LIKE '%unsigned'
  AND is_nullable = 'NO'
  AND column_default = '1'
  AND column_comment = ?`,
		table, optionMigrationMarkerCol, marker,
	).Scan(&columns).Error; err != nil {
		return false, fmt.Errorf("inspect MySQL migration marker: %w", err)
	}
	if columns == 1 {
		return true, nil
	}
	var namedColumns int64
	if err := db.Raw(`
SELECT count(*)
FROM information_schema.columns
WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`,
		table, optionMigrationMarkerCol,
	).Scan(&namedColumns).Error; err != nil {
		return false, fmt.Errorf("inspect MySQL migration marker: %w", err)
	}
	if namedColumns != 0 {
		return false, fmt.Errorf("options migration artifact collision: marker column=%s", optionMigrationMarkerCol)
	}
	return false, nil
}

func addMySQLOptionMigrationMarker(db *gorm.DB, table, marker string) error {
	if !optionSafeIdent(table) {
		return fmt.Errorf("add MySQL migration marker: unsafe table name")
	}
	return db.Exec(
		"ALTER TABLE " + quoteMySQLIdent(table) +
			" ADD COLUMN " + quoteMySQLIdent(optionMigrationMarkerCol) +
			" TINYINT UNSIGNED NOT NULL DEFAULT 1 COMMENT '" + marker + "'",
	).Error
}

func createOwnedMySQLOptionMigrationTable(
	db *gorm.DB,
	sourceDDL string,
	artifacts mysqlOptionMigrationArtifacts,
) error {
	createDDL, ok := buildOwnedMySQLOptionCreateDDL(sourceDDL, artifacts)
	if !ok {
		return fmt.Errorf("parse MySQL options CREATE TABLE")
	}
	if err := db.Exec(createDDL).Error; err != nil {
		return err
	}
	return nil
}

func showCreateMySQLTable(db *gorm.DB, tableName string) (string, error) {
	if !optionSafeIdent(tableName) {
		return "", fmt.Errorf("inspect MySQL options CREATE TABLE: unsafe table name")
	}
	ctx := db.Statement.Context
	if ctx == nil {
		ctx = context.Background()
	}
	var table, ddl string
	if err := db.Statement.ConnPool.QueryRowContext(
		ctx, "SHOW CREATE TABLE "+quoteMySQLIdent(tableName),
	).Scan(&table, &ddl); err != nil {
		return "", fmt.Errorf("inspect MySQL options CREATE TABLE: %w", err)
	}
	if table != tableName {
		return "", fmt.Errorf("inspect MySQL options CREATE TABLE: unexpected table")
	}
	return ddl, nil
}

func replaceMySQLCreateTableName(sourceDDL, from, to string) (string, bool) {
	if !optionSafeIdent(from) || !optionSafeIdent(to) {
		return "", false
	}
	position := skipMySQLSpace(sourceDDL, 0)
	next, ok := consumeMySQLKeyword(sourceDDL, position, "CREATE")
	if !ok {
		return "", false
	}
	position = skipMySQLSpace(sourceDDL, next)
	next, ok = consumeMySQLKeyword(sourceDDL, position, "TABLE")
	if !ok {
		return "", false
	}
	identifierStart := skipMySQLSpace(sourceDDL, next)
	table, identifierEnd, ok := parseMySQLQuotedIdentifier(sourceDDL, identifierStart)
	if !ok || table != from {
		return "", false
	}
	return sourceDDL[:identifierStart] + quoteMySQLIdent(to) + sourceDDL[identifierEnd:], true
}

func buildOwnedMySQLOptionCreateDDL(
	sourceDDL string,
	artifacts mysqlOptionMigrationArtifacts,
) (string, bool) {
	if !optionSafeIdent(artifacts.TmpTable) ||
		artifacts.Marker != optionMigrationMarker+"/"+artifacts.ID ||
		!isMySQLOptionMigrationArtifactID(artifacts.ID) ||
		len(artifacts.SourceSchemaSHA256) != sha256.Size*2 ||
		strings.ContainsFunc(artifacts.SourceSchemaSHA256, func(r rune) bool {
			return r < '0' || r > '9' && r < 'a' || r > 'f'
		}) {
		return "", false
	}
	position := skipMySQLSpace(sourceDDL, 0)
	next, ok := consumeMySQLKeyword(sourceDDL, position, "CREATE")
	if !ok {
		return "", false
	}
	position = skipMySQLSpace(sourceDDL, next)
	next, ok = consumeMySQLKeyword(sourceDDL, position, "TABLE")
	if !ok {
		return "", false
	}
	identifierStart := skipMySQLSpace(sourceDDL, next)
	table, identifierEnd, ok := parseMySQLQuotedIdentifier(sourceDDL, identifierStart)
	if !ok || table != "options" {
		return "", false
	}
	open := skipMySQLSpace(sourceDDL, identifierEnd)
	if open >= len(sourceDDL) || sourceDDL[open] != '(' {
		return "", false
	}
	close, ok := matchingMySQLDDLParen(sourceDDL, open)
	if !ok || strings.TrimSpace(sourceDDL[open+1:close]) == "" {
		return "", false
	}
	suffix := strings.TrimSpace(sourceDDL[close+1:])
	if len(suffix) < len("ENGINE=") ||
		!strings.EqualFold(suffix[:len("ENGINE=")], "ENGINE=") ||
		!safeMySQLDDLSuffix(suffix) ||
		strings.ContainsRune(sourceDDL, '\x00') {
		return "", false
	}
	markerDefinition := ",\n  " + quoteMySQLIdent(optionMigrationMarkerCol) +
		" TINYINT UNSIGNED NOT NULL DEFAULT 1 COMMENT '" + artifacts.Marker + "',\n  " +
		quoteMySQLIdent(optionMigrationStateCol) +
		fmt.Sprintf(" VARCHAR(%d) NOT NULL DEFAULT '", optionMigrationStateSize) +
		mysqlOptionMigrationState{
			Phase:              optionMigrationPrepared,
			SourceSchemaSHA256: artifacts.SourceSchemaSHA256,
			ActiveSchemaSHA256: strings.Repeat("0", sha256.Size*2),
		}.value() + "' COMMENT '" + optionMigrationStateMarker + "'"
	return sourceDDL[:identifierStart] +
		quoteMySQLIdent(artifacts.TmpTable) +
		sourceDDL[identifierEnd:close] +
		markerDefinition +
		sourceDDL[close:], true
}

func safeMySQLDDLSuffix(suffix string) bool {
	quote := byte(0)
	lineComment := false
	blockComment := false
	for i := 0; i < len(suffix); i++ {
		current := suffix[i]
		if lineComment {
			if current == '\n' {
				lineComment = false
			}
			continue
		}
		if blockComment {
			if current == '*' && i+1 < len(suffix) && suffix[i+1] == '/' {
				blockComment = false
				i++
			}
			continue
		}
		if quote != 0 {
			if current == '\\' && quote != '`' {
				i++
				if i >= len(suffix) {
					return false
				}
				continue
			}
			if current != quote {
				continue
			}
			if i+1 < len(suffix) && suffix[i+1] == quote {
				i++
				continue
			}
			quote = 0
			continue
		}
		switch current {
		case '\'', '"', '`':
			quote = current
		case ';':
			return false
		case '#':
			lineComment = true
		case '-':
			if i+2 < len(suffix) && suffix[i+1] == '-' &&
				(suffix[i+2] == ' ' || suffix[i+2] == '\t' ||
					suffix[i+2] == '\r' || suffix[i+2] == '\n') {
				lineComment = true
				i++
			}
		case '/':
			if i+1 < len(suffix) && suffix[i+1] == '*' {
				blockComment = true
				i++
			}
		}
	}
	return quote == 0 && !blockComment
}

func skipMySQLSpace(statement string, position int) int {
	for position < len(statement) {
		switch statement[position] {
		case ' ', '\t', '\r', '\n':
			position++
		default:
			return position
		}
	}
	return position
}

func consumeMySQLKeyword(statement string, position int, keyword string) (int, bool) {
	end := position + len(keyword)
	if end > len(statement) || !strings.EqualFold(statement[position:end], keyword) {
		return position, false
	}
	if end < len(statement) && sqliteIdentByte(statement[end]) {
		return position, false
	}
	return end, true
}

func parseMySQLQuotedIdentifier(statement string, position int) (string, int, bool) {
	if position >= len(statement) || statement[position] != '`' {
		return "", position, false
	}
	var identifier strings.Builder
	for position++; position < len(statement); position++ {
		if statement[position] != '`' {
			identifier.WriteByte(statement[position])
			continue
		}
		if position+1 < len(statement) && statement[position+1] == '`' {
			identifier.WriteByte('`')
			position++
			continue
		}
		return identifier.String(), position + 1, true
	}
	return "", position, false
}

func matchingMySQLDDLParen(statement string, open int) (int, bool) {
	depth := 0
	quote := byte(0)
	lineComment := false
	blockComment := false
	for i := open; i < len(statement); i++ {
		current := statement[i]
		if lineComment {
			if current == '\n' {
				lineComment = false
			}
			continue
		}
		if blockComment {
			if current == '*' && i+1 < len(statement) && statement[i+1] == '/' {
				blockComment = false
				i++
			}
			continue
		}
		if quote != 0 {
			if current == '\\' && quote != '`' {
				i++
				if i >= len(statement) {
					return 0, false
				}
				continue
			}
			if current != quote {
				continue
			}
			if i+1 < len(statement) && statement[i+1] == quote {
				i++
				continue
			}
			quote = 0
			continue
		}
		switch current {
		case '\'', '"', '`':
			quote = current
		case '#':
			lineComment = true
		case '-':
			if i+2 < len(statement) && statement[i+1] == '-' &&
				(statement[i+2] == ' ' || statement[i+2] == '\t' ||
					statement[i+2] == '\r' || statement[i+2] == '\n') {
				lineComment = true
				i++
			}
		case '/':
			if i+1 < len(statement) && statement[i+1] == '*' {
				blockComment = true
				i++
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i, true
			}
			if depth < 0 {
				return 0, false
			}
		}
	}
	return 0, false
}

func isOptionLegacyTable(table string) bool {
	if !strings.HasPrefix(table, optionLegacyTablePrefix) {
		return false
	}
	suffix := strings.TrimPrefix(table, optionLegacyTablePrefix)
	if isMySQLOptionMigrationArtifactID(suffix) {
		return true
	}
	return suffix != "" && !strings.ContainsFunc(suffix, func(r rune) bool {
		return r < '0' || r > '9'
	})
}

func normalizeMySQLTriggerAction(action string) string {
	return strings.ToLower(strings.Join(strings.Fields(action), " "))
}

func mysqlWritableOptionColumns(db *gorm.DB) ([]string, error) {
	return mysqlWritableOptionColumnsFromTable(db, "options")
}

func mysqlWritableOptionColumnsFromTable(db *gorm.DB, table string) ([]string, error) {
	if !optionSafeIdent(table) {
		return nil, fmt.Errorf("inspect options columns: unsafe table name")
	}
	var metadata []mysqlOptionColumn
	if err := db.Raw(`
SELECT COLUMN_NAME, EXTRA
FROM information_schema.columns
WHERE table_schema = DATABASE() AND table_name = ?
ORDER BY ORDINAL_POSITION`, table).Scan(&metadata).Error; err != nil {
		return nil, fmt.Errorf("inspect options columns: %w", err)
	}
	columns := make([]string, 0, len(metadata))
	hasKey, hasValue := false, false
	for _, column := range metadata {
		if column.Name == optionMigrationMarkerCol || column.Name == optionMigrationStateCol {
			continue
		}
		if strings.Contains(strings.ToUpper(column.Extra), "GENERATED") {
			continue
		}
		columns = append(columns, column.Name)
		hasKey = hasKey || column.Name == "key"
		hasValue = hasValue || column.Name == "value"
	}
	if !hasKey || !hasValue {
		return nil, fmt.Errorf("options source schema must contain writable key and value columns")
	}
	return columns, nil
}

func validateMySQLOptionMigrationSchema(db *gorm.DB) error {
	if err := validateMySQLGlobalTableMetadataVisibility(db); err != nil {
		return err
	}
	var metadata struct {
		HasForeignKey bool `gorm:"column:has_foreign_key"`
		HasTrigger    bool `gorm:"column:has_trigger"`
	}
	if err := db.Raw(`
SELECT
  EXISTS (
    SELECT 1
    FROM information_schema.key_column_usage
    WHERE referenced_table_name IS NOT NULL
      AND (
        (table_schema = DATABASE() AND table_name = 'options')
        OR
        (referenced_table_schema = DATABASE() AND referenced_table_name = 'options')
      )
  ) AS has_foreign_key,
  EXISTS (
    SELECT 1
    FROM information_schema.triggers
    WHERE trigger_schema = DATABASE() AND event_object_table = 'options'
  ) AS has_trigger`).Scan(&metadata).Error; err != nil {
		return fmt.Errorf("inspect MySQL options schema metadata: %w", err)
	}
	if metadata.HasForeignKey || metadata.HasTrigger {
		return fmt.Errorf("MySQL options schema cannot be preserved safely")
	}
	return nil
}

func validateMySQLOptionDeduplicationColumns(db *gorm.DB, columns []string) error {
	conflicts := make([]string, 0, len(columns)-1)
	for _, column := range columns {
		if column == "key" || column == "value" {
			continue
		}
		quoted := quoteMySQLIdent(column)
		conflicts = append(conflicts,
			"(COUNT(DISTINCT CAST("+quoted+" AS BINARY)) > 1 OR "+
				"(COUNT("+quoted+") > 0 AND COUNT("+quoted+") < COUNT(*)))",
		)
	}
	if len(conflicts) == 0 {
		return nil
	}
	var hasConflict bool
	statement := "SELECT EXISTS (" +
		"SELECT 1 FROM `options` GROUP BY `key` HAVING " +
		strings.Join(conflicts, " OR ") + " LIMIT 1) AS has_conflict"
	if err := db.Raw(statement).Scan(&hasConflict).Error; err != nil {
		return fmt.Errorf("inspect MySQL options extended columns: %w", err)
	}
	if hasConflict {
		return fmt.Errorf("MySQL options extended columns cannot be deduplicated safely")
	}
	return nil
}

func mysqlOptionColumnDifferenceComparisons(columns []string, left, right string) []string {
	comparisons := make([]string, 0, len(columns)-1)
	for _, column := range columns {
		if column == "key" {
			continue
		}
		quoted := quoteMySQLIdent(column)
		comparisons = append(comparisons,
			"NOT (CAST("+left+"."+quoted+" AS BINARY) <=> CAST("+right+"."+quoted+" AS BINARY))",
		)
	}
	return comparisons
}

func createMySQLOptionMigrationTriggers(
	db *gorm.DB,
	columns []string,
	artifacts mysqlOptionMigrationArtifacts,
) error {
	for _, trigger := range mysqlOptionMigrationTriggerSpecs(columns, artifacts) {
		statement := "CREATE TRIGGER `" + trigger.Name + "` AFTER " + trigger.Event +
			" ON `options` FOR EACH ROW " + trigger.Action
		if err := db.Exec(statement).Error; err != nil {
			_ = dropMySQLOptionMigrationTriggers(db, artifacts)
			return fmt.Errorf("create options migration trigger %s: %w", trigger.Name, err)
		}
	}
	return nil
}

func mysqlOptionMigrationTriggerSpecs(
	columns []string,
	artifacts mysqlOptionMigrationArtifacts,
) []mysqlOptionTriggerSpec {
	quoted := make([]string, len(columns))
	newValues := make([]string, len(columns))
	updates := make([]string, 0, len(columns)-1)
	for i, column := range columns {
		quoted[i] = quoteMySQLIdent(column)
		newValues[i] = "NEW." + quoted[i]
		if column != "key" {
			updates = append(updates, quoted[i]+" = NEW."+quoted[i])
		}
	}
	if len(updates) == 0 {
		updates = append(updates, "`key` = NEW.`key`")
	}
	upsert := "INSERT INTO " + quoteMySQLIdent(artifacts.TmpTable) + " (" + strings.Join(quoted, ", ") + ") VALUES (" +
		strings.Join(newValues, ", ") + ") ON DUPLICATE KEY UPDATE " + strings.Join(updates, ", ")
	conflictingColumns := mysqlOptionColumnDifferenceComparisons(columns, "source_row", "NEW")
	rejectConflictingClass := "IF EXISTS (" +
		"SELECT 1 FROM `options` AS source_row " +
		"WHERE source_row.`key` = NEW.`key` " +
		"AND (" + strings.Join(conflictingColumns, " OR ") + ") LIMIT 1" +
		") THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'options migration value conflict'; END IF; "
	rejectNull := "IF NEW.`key` IS NULL OR NEW.`value` IS NULL " +
		"THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'options migration null key or value'; END IF; "
	ownerMarker := "IF BINARY '" + artifacts.Marker + "' <> BINARY '" + artifacts.Marker +
		"' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'options migration owner marker'; END IF; "
	return []mysqlOptionTriggerSpec{
		{
			Name:  artifacts.DeleteTrigger,
			Event: "DELETE",
			Action: "BEGIN " + ownerMarker +
				"DELETE FROM " + quoteMySQLIdent(artifacts.TmpTable) + " WHERE `key` = OLD.`key` AND NOT EXISTS (" +
				"SELECT 1 FROM `options` WHERE `key` = OLD.`key` LIMIT 1); END",
		},
		{
			Name:  artifacts.UpdateTrigger,
			Event: "UPDATE",
			Action: "BEGIN " + rejectNull + ownerMarker +
				"IF NOT (OLD.`key` <=> NEW.`key`) AND NOT EXISTS (" +
				"SELECT 1 FROM `options` WHERE `key` = OLD.`key` LIMIT 1" +
				") THEN DELETE FROM " + quoteMySQLIdent(artifacts.TmpTable) + " WHERE `key` = OLD.`key`; END IF; " +
				rejectConflictingClass + upsert + "; END",
		},
		{
			Name:   artifacts.InsertTrigger,
			Event:  "INSERT",
			Action: "BEGIN " + rejectNull + ownerMarker + rejectConflictingClass + upsert + "; END",
		},
	}
}

func insertMySQLDedupedOptionRows(db *gorm.DB, columns []string, rows []Option, tmpTable string) error {
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = quoteMySQLIdent(column)
	}
	columnList := strings.Join(quoted, ", ")
	statement := "INSERT INTO " + quoteMySQLIdent(tmpTable) + " (" + columnList + ") SELECT " + columnList +
		mysqlExactOptionRowSelection("options") +
		" ON DUPLICATE KEY UPDATE `key` = VALUES(`key`)"
	for _, row := range rows {
		result := db.Exec(statement, len([]byte(row.Key)), row.Key)
		if result.Error != nil {
			return fmt.Errorf("insert rebuilt options: %w", result.Error)
		}
	}
	return nil
}

func mysqlExactOptionRowSelection(table string) string {
	return " FROM " + quoteMySQLIdent(table) +
		" WHERE OCTET_LENGTH(`key`) = ? AND BINARY `key` = BINARY ? LIMIT 1"
}

func quoteMySQLIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func readOptionMigrationRows(db *gorm.DB, sourceRowCount int64) ([]optionMigrationRow, error) {
	var rows []optionMigrationRow
	if db.Dialector.Name() == "mysql" {
		return readMySQLOptionMigrationRows(db, "options", sourceRowCount)
	} else {
		var options []struct {
			Key   sql.NullString `gorm:"column:key"`
			Value sql.NullString `gorm:"column:value"`
		}
		if err := db.Table("options").Select("key", "value").
			Order(clause.OrderByColumn{Column: clause.Column{Name: "key"}}).
			Limit(optionMigrationMaxRows + 1).Find(&options).Error; err != nil {
			return nil, fmt.Errorf("query failed: table=options source_rows=%d", sourceRowCount)
		}
		rows = make([]optionMigrationRow, len(options))
		for i, option := range options {
			rows[i] = optionMigrationRow{
				Key:            option.Key.String,
				Value:          option.Value,
				EquivalenceKey: []byte(option.Key.String),
			}
		}
	}
	if int64(len(rows)) != sourceRowCount {
		return nil, fmt.Errorf(
			"row count mismatch: table=options source_rows=%d observed_rows=%d",
			sourceRowCount, len(rows),
		)
	}
	return rows, nil
}

func readMySQLOptionMigrationRows(
	db *gorm.DB,
	table string,
	sourceRowCount int64,
) ([]optionMigrationRow, error) {
	if !optionSafeIdent(table) {
		return nil, fmt.Errorf("query failed: unsafe options table")
	}
	quotedTable := quoteMySQLIdent(table)
	query := "SELECT COALESCE(source_row.`key`, '') AS `key`, source_row.`value`,\n" +
		"       COALESCE(key_class.equivalence_key, CAST('' AS BINARY)) AS equivalence_key\n" +
		"FROM " + quotedTable + " AS source_row\n" +
		"JOIN (\n" +
		"  SELECT `key` AS representative_key,\n" +
		"         MIN(CAST(`key` AS BINARY)) AS equivalence_key\n" +
		"  FROM " + quotedTable + "\n" +
		"  GROUP BY `key`\n" +
		") AS key_class ON source_row.`key` <=> key_class.representative_key\n" +
		"ORDER BY equivalence_key, BINARY `key`, BINARY `value`\n" +
		"LIMIT ?"
	var rows []optionMigrationRow
	if err := db.Raw(query, optionMigrationMaxRows+1).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf(
			"query failed: table=%s source_rows=%d",
			table, sourceRowCount,
		)
	}
	if int64(len(rows)) != sourceRowCount {
		return nil, fmt.Errorf(
			"row count mismatch: table=%s source_rows=%d observed_rows=%d",
			table, sourceRowCount, len(rows),
		)
	}
	return rows, nil
}

func dedupeOptionRows(rows []optionMigrationRow, digestKey []byte) ([]Option, error) {
	rowsByClass := make(map[string][]optionMigrationRow)
	for _, row := range rows {
		classKey := string(row.EquivalenceKey)
		rowsByClass[classKey] = append(rowsByClass[classKey], row)
	}
	classKeys := slices.Sorted(maps.Keys(rowsByClass))
	deduped := make([]Option, 0, len(classKeys))
	for _, classKey := range classKeys {
		classRows := rowsByClass[classKey]
		key := canonicalOptionKey(classRows)
		values := make([]sql.NullString, len(classRows))
		for i, row := range classRows {
			values[i] = row.Value
		}
		slices.SortFunc(values, func(left, right sql.NullString) int {
			if left.Valid != right.Valid {
				if !left.Valid {
					return -1
				}
				return 1
			}
			return strings.Compare(left.String, right.String)
		})
		distinctValues := slices.Compact(slices.Clone(values))
		if key == "" || len(distinctValues) != 1 || !distinctValues[0].Valid {
			return nil, fmt.Errorf(
				"options primary key conflict: key=%q rows=%d value_digest=hmac-sha256:%s",
				key, len(classRows), optionValuesDigest(digestKey, values),
			)
		}
		deduped = append(deduped, Option{Key: key, Value: distinctValues[0].String})
	}
	return deduped, nil
}

func canonicalOptionKey(rows []optionMigrationRow) string {
	for _, group := range protectedOptionGroups() {
		for _, protectedKey := range group.keys {
			if slices.ContainsFunc(rows, func(row optionMigrationRow) bool {
				return row.Key == protectedKey
			}) {
				return protectedKey
			}
		}
	}
	keys := make([]string, len(rows))
	for i, row := range rows {
		keys[i] = row.Key
	}
	slices.Sort(keys)
	return keys[0]
}

// optionValuesDigest authenticates an unambiguous encoding of the complete
// sorted value multiset: domain tag, value count, then type tag and value.
func optionValuesDigest(key []byte, values []sql.NullString) string {
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write([]byte("new-api/options-conflict/v2\x00"))
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(values)))
	_, _ = hash.Write(length[:])
	for _, value := range values {
		if !value.Valid {
			_, _ = hash.Write([]byte{0})
			continue
		}
		_, _ = hash.Write([]byte{1})
		binary.BigEndian.PutUint64(length[:], uint64(len(value.String)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value.String))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func swapOptionTables(db *gorm.DB, tmp, backup string) error {
	if !optionSafeIdent(tmp) || !optionSafeIdent(backup) {
		return fmt.Errorf("unsafe options table name")
	}
	if db.Dialector.Name() == "mysql" {
		sql := "RENAME TABLE `options` TO `" + backup + "`, `" + tmp + "` TO `options`"
		if err := db.Exec(sql).Error; err != nil {
			return fmt.Errorf("rename options tables: %w", err)
		}
		return nil
	}
	if err := db.Migrator().RenameTable("options", backup); err != nil {
		return fmt.Errorf("rename options to %s: %w", backup, err)
	}
	if err := db.Migrator().RenameTable(tmp, "options"); err != nil {
		return fmt.Errorf("rename %s to options: %w", tmp, err)
	}
	return nil
}

func optionSafeIdent(name string) bool {
	if name == "" {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool {
		return r != '_' && (r < '0' || r > '9') && (r < 'a' || r > 'z')
	})
}
