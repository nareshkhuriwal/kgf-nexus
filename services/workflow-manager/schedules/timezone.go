package schedules

import (
	"fmt"

	"github.com/khuriwalgroup/kgf-nexus/internal/cron"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// parseIn compiles a schedule's cron expression in the schedule's own timezone.
//
// One helper rather than eight call sites each loading a location, because the failure
// mode of getting it wrong in ONE of them is the worst kind: the schedule fires, on the
// wrong hour, for half the year, with nothing reporting it. A shared helper means the
// zone is either applied everywhere or nowhere.
//
// A schedule with an unloadable timezone is a hard error rather than a UTC fallback.
// Falling back would make a London schedule silently run an hour early half the year --
// exactly the bug timezone support exists to fix, reintroduced as "graceful"
// degradation. Refusing means the schedule stops and says why, which an operator can
// act on.
func parseIn(sch store.Schedule) (*cron.Schedule, error) {
	loc, err := cron.LoadLocation(sch.Timezone)
	if err != nil {
		return nil, fmt.Errorf("schedule %s/%s: %w", sch.Namespace, sch.Name, err)
	}
	expr, err := cron.ParseIn(sch.CronExpr, loc)
	if err != nil {
		return nil, fmt.Errorf("schedule %s/%s has an unparseable cron %q: %w",
			sch.Namespace, sch.Name, sch.CronExpr, err)
	}
	return expr, nil
}
