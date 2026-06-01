package sync

import (
	"log"
	"strings"

	"github.com/mevdschee/github-export/internal/github"
)

// featureScopes maps each optional feature to the OAuth scopes that unlock it
// (holding any one is enough). Features absent from this map need only `repo`,
// without which nothing syncs at all. Projects v2 is the odd one out: GitHub
// rejects the query field-by-field ("the 'createdAt' field requires one of the
// following scopes: ['read:project']") when the scope is missing, so we skip
// the feature up front instead of letting it spew one error per field.
var featureScopes = map[string][]string{
	"projects": {"read:project", "project"},
}

// checkScopes inspects the token's granted scopes, logs them, and returns the
// set of features to skip because none of their accepted scopes is present.
// It returns nil (skip nothing) for tokens that do not report scopes —
// fine-grained PATs and GitHub App tokens — since their permissions are not
// expressed as a scope list and must not be second-guessed.
func checkScopes(c *github.Client) map[string]bool {
	granted, known := c.Scopes()
	if !known {
		return nil
	}
	if len(granted) == 0 {
		log.Println("Token scopes: (none)")
	} else {
		log.Printf("Token scopes: %s", strings.Join(granted, ", "))
	}

	have := make(map[string]bool, len(granted))
	for _, s := range granted {
		have[s] = true
	}

	skip := map[string]bool{}
	for feature, accepted := range featureScopes {
		ok := false
		for _, s := range accepted {
			if have[s] {
				ok = true
				break
			}
		}
		if !ok {
			skip[feature] = true
			log.Printf("Skipping %s: token lacks the %q scope (add it with: gh auth refresh -s %s)",
				feature, accepted[0], accepted[0])
		}
	}
	return skip
}
