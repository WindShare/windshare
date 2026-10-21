package turnpath

import (
	"github.com/windshare/windshare/core/session/turncontrol"
)

// Reports are cumulative observations, not commands. Only known grant
// authorities own storage, and each path retains its highest reported value.
func (s *Sender) retainReport(report turncontrol.Report) {
	s.mu.Lock()
	path := s.paths[report.PathID]
	advanced := path != nil && path.grant.Authority == report.Authority && report.UsefulBytes > path.reported
	if advanced {
		path.reported = report.UsefulBytes
	}
	s.mu.Unlock()
	if advanced {
		s.wake()
	}
}

func (s *Sender) applyReports() {
	for _, service := range s.services {
		for _, path := range []*delegation{service.current, service.pending} {
			if path != nil {
				s.applyReport(path)
			}
		}
	}
}

func (s *Sender) applyReport(path *delegation) {
	s.mu.Lock()
	reported := path.reported
	s.mu.Unlock()
	if reported > path.useful {
		path.lease.Charge(Usage{Useful: reported - path.useful})
		path.useful = reported
		s.recoverBudgetAfterProgress(path)
		s.observe(path, MilestoneUsefulContent, "")
	}
}
