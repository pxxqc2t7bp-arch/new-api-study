package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

type UpstreamDailySummary struct {
	Pricing          OfficialPricingSyncSummary `json:"pricing"`
	Routing          UpstreamReconcileSummary   `json:"routing"`
	DeletedSnapshots int64                      `json:"deleted_snapshots"`
	BarkSent         bool                       `json:"bark_sent"`
	BarkError        string                     `json:"bark_error,omitempty"`
}

func RunUpstreamDailyMaintenance(ctx context.Context, now time.Time) (UpstreamDailySummary, error) {
	var summary UpstreamDailySummary
	pricing, err := RunOfficialPricingSync(ctx, now)
	if err != nil {
		return summary, err
	}
	summary.Pricing = pricing
	routing, err := ReconcileManagedUpstreams(now)
	if err != nil {
		return summary, err
	}
	summary.Routing = routing
	cutoff := now.AddDate(0, 0, -operation_setting.GetUpstreamOrchestrationSetting().DetailRetentionDays).Unix()
	deleted := model.DB.Where("is_daily_rollup = ? AND observed_at < ?", false, cutoff).
		Delete(&model.UpstreamMetricSnapshot{})
	if deleted.Error != nil {
		return summary, deleted.Error
	}
	summary.DeletedSnapshots = deleted.RowsAffected
	if err := sendUpstreamDailyBark(now, pricing, routing); err != nil {
		summary.BarkError = err.Error()
		common.SysLog("upstream daily Bark skipped: " + err.Error())
	} else {
		summary.BarkSent = true
	}
	return summary, nil
}

func sendUpstreamDailyBark(now time.Time, pricing OfficialPricingSyncSummary, routing UpstreamReconcileSummary) error {
	sources, err := model.ListUpstreamSources()
	if err != nil {
		return err
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("日期：%s", now.In(time.FixedZone("Asia/Shanghai", 8*60*60)).Format("2006-01-02")))
	for _, source := range sources {
		balance := "N/A"
		if source.Balance != nil {
			balance = fmt.Sprintf("$%.2f", *source.Balance)
		}
		lines = append(lines, fmt.Sprintf("%s：%s，余额 %s，快照 %s",
			source.Name,
			source.Status,
			balance,
			time.Unix(source.LastSnapshotAt, 0).Format("01-02 15:04"),
		))
	}
	groups, _ := model.ListUpstreamGroups()
	routes, _ := model.ListUpstreamManagedRoutes()
	sourceByID := make(map[int64]model.UpstreamSource, len(sources))
	for _, source := range sources {
		sourceByID[source.ID] = source
	}
	groupByIdentity := make(map[string]model.UpstreamGroup, len(groups))
	for _, group := range groups {
		groupByIdentity[upstreamGroupIdentity(group.SourceID, group.ExternalID)] = group
	}
	for _, route := range routes {
		if route.State != model.UpstreamRouteStateActive {
			continue
		}
		source := sourceByID[route.SourceID]
		group := groupByIdentity[upstreamGroupIdentity(route.SourceID, route.ExternalGroupID)]
		lines = append(lines, fmt.Sprintf(
			"#%d %s/%s %s %.4fx",
			route.ChannelID,
			source.Key,
			group.Name,
			route.Protocol,
			route.EffectiveMultiplier,
		))
	}
	lines = append(lines,
		fmt.Sprintf("价格：应用 %d，拒绝 %d，未变化 %d", pricing.Applied, pricing.Rejected, pricing.Unchanged),
		fmt.Sprintf("路由：激活 %d，隔离 %d，长期关闭 %d，新建队列 %d",
			routing.RoutesActivated,
			routing.RoutesQuarantined,
			routing.RoutesLongRed,
			routing.EnrollmentQueued,
		),
	)
	return NotifyRootBark(
		dto.NotifyTypeChannelUpdate+"_upstream_daily",
		"New API 上游编排日报",
		strings.Join(lines, "\n"),
	)
}
