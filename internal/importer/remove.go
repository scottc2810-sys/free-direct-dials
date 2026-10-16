package importer

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"markemind/dials/internal/db"
	"markemind/dials/internal/emailcheck"
	"markemind/dials/internal/filter"
	"markemind/dials/internal/normalize"
)

// FindByIdentity locates contacts for a removal request. kind is email,
// linkedin or phone. Phone matching scans (admin-only, rare).
func FindByIdentity(ctx context.Context, q db.Querier, kind, value string) ([]int64, error) {
	var query string
	var arg any
	switch kind {
	case "email":
		e, ok, _ := emailcheck.Syntax(value)
		if !ok {
			return nil, errors.New("invalid email")
		}
		query, arg = `SELECT id FROM contact_info WHERE email_key=?`, normalize.EmailKey(e)
	case "linkedin":
		k := normalize.LinkedInKey(value)
		if k == "" {
			return nil, errors.New("invalid LinkedIn profile URL")
		}
		query, arg = `SELECT id FROM contact_info WHERE li_key=?`, k
	case "phone":
		d := normalize.PhoneDigits(value)
		if len(d) < 6 {
			return nil, errors.New("invalid phone number")
		}
		query = `SELECT id FROM contact_info WHERE phone<>'' AND replace(replace(replace(replace(replace(replace(phone,' ',''),'-',''),'(',''),')',''),'+',''),'.','') LIKE ?`
		arg = "%" + d
	default:
		return nil, errors.New("unknown identity kind")
	}
	rows, err := q.QueryContext(ctx, query, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ContactKeys returns the suppression keys of contacts (call before deleting them).
func ContactKeys(ctx context.Context, q db.Querier, ids []int64) ([]string, error) {
	var keys []string
	for len(ids) > 0 {
		n := min(500, len(ids))
		args := make([]any, n)
		for i, id := range ids[:n] {
			args[i] = id
		}
		ids = ids[n:]
		rows, err := q.QueryContext(ctx, `SELECT email_key, li_key, h_key FROM contact_info WHERE id IN (`+db.Placeholders(n)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var ek, lk, hk sql.NullString
			if err := rows.Scan(&ek, &lk, &hk); err != nil {
				rows.Close()
				return nil, err
			}
			keys = append(keys, SuppressKeys(ek.String, lk.String, hk.String)...)
		}
		rows.Close()
	}
	return keys, nil
}

// AddSuppress records identity keys that future uploads must skip.
func AddSuppress(ctx context.Context, d *db.DB, keys []string, now int64) error {
	if len(keys) == 0 {
		return nil
	}
	return d.Tx(ctx, func(tx *sql.Tx) error {
		for _, k := range keys {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO suppress(k, ts) VALUES(?,?)`, k, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteContacts removes contacts from one SQLite store (search indexes and
// unlocks included) and keeps per-value counts exact. Suppress their keys
// first (ContactKeys + AddSuppress) so a re-upload can't bring them back.
func DeleteContacts(ctx context.Context, d *db.DB, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	deleted := 0
	cols := make([]string, len(filter.Dims))
	for i, dm := range filter.Dims {
		cols[i] = dm.Key
	}
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			var dims [12]int64
			dst := make([]any, len(dims))
			for i := range dims {
				dst[i] = &dims[i]
			}
			err := tx.QueryRowContext(ctx, `SELECT `+strings.Join(cols, ",")+` FROM contacts WHERE id=?`, id).Scan(dst...)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			for _, q := range []string{
				`DELETE FROM fts WHERE rowid=?`, `DELETE FROM tech_fts WHERE rowid=?`,
				`DELETE FROM unlocked WHERE contact_id=?`, `DELETE FROM contact_info WHERE id=?`, `DELETE FROM contacts WHERE id=?`,
			} {
				if _, err := tx.ExecContext(ctx, q, id); err != nil {
					return err
				}
			}
			args := make([]any, len(dims))
			for i, v := range dims {
				args[i] = v
			}
			if _, err := tx.ExecContext(ctx, `UPDATE dims SET cnt=cnt-1 WHERE cnt>0 AND id IN (`+db.Placeholders(len(dims))+`)`, args...); err != nil {
				return err
			}
			deleted++
		}
		return nil
	})
	if err != nil || deleted == 0 {
		return deleted, err
	}
	if err := RefreshStats(ctx, d); err != nil {
		return deleted, err
	}
	_, err = d.BumpDataset(ctx)
	return deleted, err
}

// Suppress adds a removal-request identity that is not (yet) in the data.
func Suppress(ctx context.Context, d *db.DB, kind, value string, now int64) error {
	var keys []string
	switch kind {
	case "email":
		if e, ok, _ := emailcheck.Syntax(value); ok {
			keys = SuppressKeys(normalize.EmailKey(e), "", "")
		}
	case "linkedin":
		keys = SuppressKeys("", normalize.LinkedInKey(value), "")
	}
	for _, k := range keys {
		if _, err := d.W.ExecContext(ctx, `INSERT OR IGNORE INTO suppress(k, ts) VALUES(?,?)`, k, now); err != nil {
			return err
		}
	}
	return nil
}
