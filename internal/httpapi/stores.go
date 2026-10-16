package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"markemind/dials/internal/auth"
	"markemind/dials/internal/store"
)

// ---------- superadmin: contact databases (Supabase / Postgres / SQLite) ----------

func (a *App) adminStores(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	writeJSON(w, http.StatusOK, map[string]any{"stores": a.Store.Statuses(r.Context()), "importing": a.Imports.Running()})
}

type storeIn struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	DSN     string `json:"dsn"`
	QuotaMB int64  `json:"quota_mb"`
	Read    bool   `json:"read"`
	Write   bool   `json:"write"`
}

func (a *App) adminStoreTest(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	var in storeIn
	if !decodeJSON(w, r, &in, 8<<10) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	p, err := a.Store.Test(ctx, in.Kind, in.DSN)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "connect_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *App) adminStoreAdd(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	var in storeIn
	if !decodeJSON(w, r, &in, 8<<10) {
		return
	}
	if in.QuotaMB < 0 || in.QuotaMB > 1<<30 {
		writeErr(w, http.StatusBadRequest, "bad_quota", "Quota must be a positive number of MB.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second) // first connect creates the schema
	defer cancel()
	m, err := a.Store.Add(ctx, in.Name, in.Kind, in.DSN, in.QuotaMB<<20)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrDuplicate) {
			code = http.StatusConflict
		}
		writeErr(w, code, "add_failed", err.Error())
		return
	}
	a.audit(r.Context(), s.Username, clientIP(r), "store.add", fmt.Sprintf("#%d %s (%s, %s)", m.ID, m.Name, m.Kind, store.DisplayHost(m.Kind, in.DSN)))
	a.purgeData(context.WithoutCancel(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": m.ID, "stores": a.Store.Statuses(r.Context())})
}

func (a *App) adminStoreUpdate(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "Unknown database.")
		return
	}
	var in storeIn
	if !decodeJSON(w, r, &in, 4<<10) {
		return
	}
	if in.QuotaMB < 0 || in.QuotaMB > 1<<30 {
		writeErr(w, http.StatusBadRequest, "bad_quota", "Quota must be a positive number of MB.")
		return
	}
	if err := a.Store.Update(r.Context(), id, in.Name, in.Read, in.Write, in.QuotaMB<<20); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, code, "update_failed", err.Error())
		return
	}
	a.audit(r.Context(), s.Username, clientIP(r), "store.update", fmt.Sprintf("#%d %s read=%v write=%v quota=%dMB", id, in.Name, in.Read, in.Write, in.QuotaMB))
	a.purgeData(context.WithoutCancel(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stores": a.Store.Statuses(r.Context())})
}

func (a *App) adminStoreDetach(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "Unknown database.")
		return
	}
	if a.Imports.Running() {
		writeErr(w, http.StatusConflict, "importing", "Wait for the running import to finish.")
		return
	}
	m := a.Store.Member(id)
	if err := a.Store.Detach(r.Context(), id); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, code, "detach_failed", err.Error())
		return
	}
	a.audit(r.Context(), s.Username, clientIP(r), "store.detach", fmt.Sprintf("#%d %s", id, m.Name))
	a.purgeData(context.WithoutCancel(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stores": a.Store.Statuses(r.Context())})
}

// adminStoreCheck runs the keepalive/size check now (it also runs every 6 hours).
func (a *App) adminStoreCheck(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	warns := a.Store.Monitor(ctx)
	var msgs []string
	for _, wn := range warns {
		if wn.Err != "" {
			msgs = append(msgs, wn.Store+": "+wn.Err)
		} else {
			msgs = append(msgs, fmt.Sprintf("%s: %d%% of quota", wn.Store, wn.Pct))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"stores": a.Store.Statuses(r.Context()), "warnings": msgs})
}
