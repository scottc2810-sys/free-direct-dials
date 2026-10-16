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

// DeleteContacts removes contacts everywhere, adds their identities to the
// suppression list (so re-uploads skip them) and keeps counts exact.
func DeleteContacts(ctx context.Context, d *db.DB, ids []int64, now int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	deleted := 0
	cols := make([]string, len(filter.Dims))
	for i, dm := range filter.Dims {
		cols[i] = "c." + dm.Key
	}
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			var ek, lk, hk sql.NullString
			var dims [12]int64
			dst := []any{&ek, &lk, &hk}
			for i := range dims {
				dst = append(dst, &dims[i])
			}
			err := tx.QueryRowContext(ctx, `SELECT i.email_key, i.li_key, i.h_key, `+strings.Join(cols, ",")+`
				FROM contacts c JOIN contact_info i ON i.id=c.id WHERE c.id=?`, id).Scan(dst...)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			for _, k := range SuppressKeys(ek.String, lk.String, hk.String) {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO suppress(k, ts) VALUES(?,?)`, k, now); err != nil {
					return err
				}
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
