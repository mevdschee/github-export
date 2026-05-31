package httpapi

import (
	"encoding/json"
	"net/http"
)

// Git content handlers. Each falls through to the proxy on a miss so unknown
// refs/paths still resolve against GitHub.

func (s *Server) handleBranches(w http.ResponseWriter, r *http.Request) {
	items, err := s.git.Branches()
	if err != nil {
		s.handleProxy(w, r)
		return
	}
	s.freshness(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(orEmpty(items))
}

func (s *Server) handleTags(w http.ResponseWriter, r *http.Request) {
	items, err := s.git.Tags()
	if err != nil {
		s.handleProxy(w, r)
		return
	}
	s.freshness(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(orEmpty(items))
}

func (s *Server) handleCommits(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	items, err := s.git.Commits(v.Get("sha"), atoiDefault(v.Get("per_page"), 30), atoiDefault(v.Get("page"), 1))
	if err != nil {
		s.handleProxy(w, r)
		return
	}
	s.freshness(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(orEmpty(items))
}

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	doc, ok, err := s.git.Commit(r.PathValue("sha"))
	if err != nil || !ok {
		s.handleProxy(w, r)
		return
	}
	s.freshness(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(doc)
}

func (s *Server) handleContents(w http.ResponseWriter, r *http.Request) {
	doc, ok, err := s.git.Contents(r.PathValue("path"), r.URL.Query().Get("ref"))
	if err != nil || !ok {
		s.handleProxy(w, r)
		return
	}
	s.freshness(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(doc)
}

func orEmpty(items []map[string]any) []map[string]any {
	if items == nil {
		return []map[string]any{}
	}
	return items
}
