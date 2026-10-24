package esapi

import (
	"net/http"

	apierr "github.com/unisearch-db/unisearch/internal/errors"
	"github.com/unisearch-db/unisearch/internal/jsonx"
	"github.com/unisearch-db/unisearch/internal/snapshot"
)

// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.6

// handleListSnapshotRepos handles GET /_snapshot (list all repos).
func (s *Server) handleListSnapshotRepos(w http.ResponseWriter, r *http.Request) {
	repos := s.snapshotRepos.List()
	writeJSON(w, 0, map[string]any{"count": len(repos), "results": repos})
}

func (s *Server) makePutSnapshotRepoHandler(repo string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			writeError(w, apierr.NewIllegalArgument("invalid body: "+err.Error()))
			return
		}
		var cfg snapshot.RepoConfig
		if err := jsonx.Unmarshal(body, &cfg); err != nil {
			writeError(w, apierr.NewIllegalArgument("invalid JSON: "+err.Error()))
			return
		}
		if cfg.Type == "" {
			cfg.Type = "fs"
		}
		s.snapshotRepos.Put(repo, &cfg)
		writeJSON(w, 0, map[string]any{"acknowledged": true})
	}
}

func (s *Server) makeGetSnapshotsHandler(repo string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snaps, err := s.snapshotRepos.ListSnapshots(repo)
		if err != nil {
			writeError(w, apierr.NewIllegalArgument(err.Error()))
			return
		}
		writeJSON(w, 0, map[string]any{"snapshots": snaps})
	}
}

func (s *Server) makeDeleteSnapshotRepoHandler(repo string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.snapshotRepos.Delete(repo) {
			writeError(w, apierr.NewIllegalArgument("repository '"+repo+"' not found"))
			return
		}
		writeJSON(w, 0, map[string]any{"acknowledged": true})
	}
}

func (s *Server) makeCreateSnapshotHandler(repo, snapshotName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// List all indices as snapshot targets.
		indices, err := s.engine.Indices(r.Context(), "*")
		if err != nil {
			writeError(w, apierr.NewInternal("failed to list indices: "+err.Error()))
			return
		}
		idxNames := make([]string, 0, len(indices))
		for _, ii := range indices {
			idxNames = append(idxNames, ii.Index)
		}
		info, err := s.snapshotRepos.CreateSnapshot(repo, snapshotName, idxNames, func(idx string) ([]byte, error) {
			return s.engine.GetMapping(r.Context(), idx)
		})
		if err != nil {
			writeError(w, apierr.NewInternal("snapshot creation failed: "+err.Error()))
			return
		}
		writeJSON(w, 0, map[string]any{"snapshot": info})
	}
}

func (s *Server) makeGetSnapshotHandler(repo, snapshotName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snaps, err := s.snapshotRepos.ListSnapshots(repo)
		if err != nil {
			writeError(w, apierr.NewIllegalArgument(err.Error()))
			return
		}
		for _, snap := range snaps {
			if snap.Snapshot == snapshotName {
				writeJSON(w, 0, map[string]any{"snapshots": []any{snap}})
				return
			}
		}
		writeError(w, apierr.NewIllegalArgument("snapshot '"+snapshotName+"' not found in repo '"+repo+"'"))
	}
}

func (s *Server) makeDeleteSnapshotHandler(repo, snapshotName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.snapshotRepos.DeleteSnapshot(repo, snapshotName); err != nil {
			writeError(w, apierr.NewIllegalArgument(err.Error()))
			return
		}
		writeJSON(w, 0, map[string]any{"acknowledged": true})
	}
}

func (s *Server) makeRestoreSnapshotHandler(repo, snapshotName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := s.snapshotRepos.RestoreSnapshot(repo, snapshotName, "", func(idx string, meta []byte) error {
			return s.engine.PutMapping(r.Context(), idx, meta)
		})
		if err != nil {
			writeError(w, apierr.NewInternal("restore failed: "+err.Error()))
			return
		}
		writeJSON(w, 0, map[string]any{"acknowledged": true})
	}
}

func (s *Server) makeSnapshotStatusHandler(repo, snapshotName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, err := s.snapshotRepos.GetSnapshot(repo, snapshotName)
		if err != nil {
			writeError(w, apierr.NewIllegalArgument(err.Error()))
			return
		}
		writeJSON(w, 0, map[string]any{"snapshots": []any{map[string]any{
			"snapshot": info.Snapshot,
			"state":    info.State,
			"indices":  info.Indices,
		}}})
	}
}

// makeVerifyRepoHandler handles POST /_snapshot/{repo}/_verify.
// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.6.3
func (s *Server) makeVerifyRepoHandler(repo string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.snapshotRepos.VerifyRepo(repo); err != nil {
			writeError(w, apierr.NewIllegalArgument(err.Error()))
			return
		}
		writeJSON(w, 0, map[string]any{
			"acknowledged": true,
			"verified":     []string{"node-1"},
		})
	}
}
