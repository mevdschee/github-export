package sync

import (
	"log"

	"github.com/mevdschee/github-export/internal/github"
	"github.com/mevdschee/github-export/internal/hooks"
	"github.com/mevdschee/github-export/internal/store"
)

// Run performs a full sync of all entities into the store and records the
// detected change events (stamped with syncStart) in the events table. It
// returns the detected events for summary logging.
//
// A failure to fetch or store an individual entity group is logged as a warning
// and the sync continues; only a failure to persist repo metadata (and the sync
// timestamp) is treated as fatal, since that is what drives incremental runs.
func Run(c *github.Client, s *store.Store, owner, repo, since, syncStart string) ([]hooks.Event, error) {
	var events []hooks.Event

	if err := Labels(c, s, owner, repo); err != nil {
		log.Printf("Warning: %v", err)
	}
	if err := Milestones(c, s, owner, repo); err != nil {
		log.Printf("Warning: %v", err)
	}
	issueProjects, projectEvents, err := Projects(c, s, owner, repo, since)
	if err != nil {
		log.Printf("Warning: %v", err)
	}
	issueEvents, err := Issues(c, s, owner, repo, since, issueProjects)
	if err != nil {
		log.Printf("Warning: %v", err)
	}
	events = append(events, issueEvents...)
	events = append(events, projectEvents...)

	releaseEvents, err := Releases(c, s, owner, repo)
	if err != nil {
		log.Printf("Warning: %v", err)
	}
	events = append(events, releaseEvents...)

	discussionEvents, err := Discussions(c, s, owner, repo, since)
	if err != nil {
		log.Printf("Warning: %v", err)
	}
	events = append(events, discussionEvents...)

	if err := Repo(c, s, owner, repo, syncStart); err != nil {
		return events, err
	}

	if err := s.InsertEvents(syncStart, events); err != nil {
		return events, err
	}

	return events, nil
}
