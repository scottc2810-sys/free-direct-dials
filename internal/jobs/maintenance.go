package jobs

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"markemind/dials/internal/db"
	"markemind/dials/internal/platform"
)

// Hourly deletes expired short-lived rows and stale upload temp files.
func Hourly(ctx context.Context, d *db.DB, clk platform.Clock, tmpDir string) error {
	now := clk.Now()
	for _, q := range []struct {
		sql string
		arg any
	}{
		{`DELETE FROM otps WHERE exp < ? AND window_start < ?`, nil},
		{`DELETE FROM sessions WHERE exp < ?`, now.Unix()},
		{`DELETE FROM admin_sessions WHERE exp < ? OR idle_exp < ?`, nil},
		{`DELETE FROM ad_tokens WHERE created < ?`, now.Add(-time.Hour).UnixMilli()},
		{`DELETE FROM user_daily WHERE day < ?`, platform.Day(now.Add(-72 * time.Hour))},
	} {
		var err error
		switch {
		case strings.HasPrefix(q.sql, "DELETE FROM otps"):
			_, err = d.W.ExecContext(ctx, q.sql, now.Unix(), now.Add(-2*time.Hour).Unix())
		case strings.HasPrefix(q.sql, "DELETE FROM admin_sessions"):
			_, err = d.W.ExecContext(ctx, q.sql, now.Unix(), now.Unix())
		default:
			_, err = d.W.ExecContext(ctx, q.sql, q.arg)
		}
		if err != nil {
			return err
		}
	}
	return CleanTmp(tmpDir, now.Add(-2*time.Hour))
}

// CleanTmp removes upload-* entries older than cutoff.
func CleanTmp(dir string, cutoff time.Time) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "upload-") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	return nil
}

// Daily folds the WAL, reclaims free pages and prunes retention-bound rows.
func Daily(ctx context.Context, d *db.DB, clk platform.Clock) error {
	now := clk.Now()
	steps := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM verdicts WHERE ts < ?`, []any{now.Add(-30 * 24 * time.Hour).Unix()}},
		{`DELETE FROM mx_cache WHERE ts < ?`, []any{now.Add(-30 * 24 * time.Hour).Unix()}},
		{`DELETE FROM outbox WHERE status IN ('sent','failed') AND COALESCE(NULLIF(sent_at,0), next_at) < ?`, []any{now.Add(-7 * 24 * time.Hour).Unix()}},
		{`DELETE FROM audit WHERE ts < ?`, []any{now.Add(-365 * 24 * time.Hour).Unix()}},
		{`PRAGMA incremental_vacuum(2000)`, nil},
		{`PRAGMA optimize`, nil},
		{`PRAGMA wal_checkpoint(TRUNCATE)`, nil},
	}
	for _, s := range steps {
		if _, err := d.W.ExecContext(ctx, s.sql, s.args...); err != nil {
			return fmt.Errorf("%s: %w", s.sql, err)
		}
	}
	return nil
}

// Monthly vacuums when worthwhile and safe, prunes long-gone users and orphans.
func Monthly(ctx context.Context, d *db.DB, clk platform.Clock, stat platform.DiskStatFunc, log *slog.Logger) error {
	now := clk.Now()
	if _, err := d.W.ExecContext(ctx, `DELETE FROM unlocked WHERE contact_id NOT IN (SELECT id FROM contacts)`); err != nil {
		return err
	}
	cutoff := now.AddDate(-2, 0, 0).Unix()
	rows, err := d.R.QueryContext(ctx, `SELECT id FROM users WHERE subscribed=0 AND last_seen < ? LIMIT 10000`, cutoff)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		for _, q := range []string{`DELETE FROM sessions WHERE user_id=?`, `DELETE FROM unlocked WHERE user_id=?`,
			`DELETE FROM user_daily WHERE user_id=?`, `DELETE FROM outbox WHERE user_id=?`, `DELETE FROM users WHERE id=?`} {
			if _, err := d.W.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
	}
	var pages, free, pageSize int64
	d.R.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages)
	d.R.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free)
	d.R.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize)
	if pages == 0 || float64(free)/float64(pages) < 0.25 {
		return nil
	}
	ds, err := stat(filepath.Dir(d.Path))
	if err != nil {
		return err
	}
	if float64(ds.Free) < 1.2*float64(pages*pageSize) {
		log.Warn("skipping VACUUM: not enough free disk", "need", pages*pageSize, "free", ds.Free)
		return nil
	}
	_, err = d.W.ExecContext(ctx, `VACUUM`)
	return err
}

// Backup snapshots the DB with VACUUM INTO, verifies it, compresses it with
// streaming zstd and keeps the newest Keep copies.
type Backup struct {
	DBPath  string
	Dir     string
	Keep    int
	Guard   *Guard
	Stat    platform.DiskStatFunc
	Clock   platform.Clock
	MinFree func(dbBytes int64) uint64 // default 1.3 × DB + 1 GB
	Key     []byte                     // optional AES-256 key: backups are encrypted at rest when set
}

type BackupInfo struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Time  int64  `json:"time"`
}

func (b *Backup) Run(ctx context.Context) (string, error) {
	if b.Guard != nil {
		if err := b.Guard.AllowHeavy(); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(b.Dir, 0o750); err != nil {
		return "", err
	}
	fi, err := os.Stat(b.DBPath)
	if err != nil {
		return "", err
	}
	need := uint64(float64(fi.Size())*1.3) + 1<<30
	if b.MinFree != nil {
		need = b.MinFree(fi.Size())
	}
	if ds, err := b.Stat(b.Dir); err == nil && ds.Free < need {
		return "", fmt.Errorf("backup skipped: need %d bytes free, have %d", need, ds.Free)
	}
	stamp := b.Clock.Now().UTC().Format("20060102-150405")
	tmp := filepath.Join(b.Dir, "tmp-"+stamp+".db")
	defer os.Remove(tmp)
	aux, err := db.OpenAux(b.DBPath)
	if err != nil {
		return "", err
	}
	_, err = aux.ExecContext(ctx, `VACUUM INTO ?`, filepath.ToSlash(tmp))
	aux.Close()
	if err != nil {
		return "", fmt.Errorf("vacuum into: %w", err)
	}
	if err := quickCheck(ctx, tmp); err != nil {
		return "", err
	}
	out := filepath.Join(b.Dir, "dials-"+stamp+".db.zst")
	if len(b.Key) > 0 {
		out += ".enc"
	}
	if err := compressFile(tmp, out+".part", b.Key); err != nil {
		os.Remove(out + ".part")
		return "", err
	}
	if err := os.Rename(out+".part", out); err != nil {
		return "", err
	}
	return out, b.prune()
}

func quickCheck(ctx context.Context, path string) error {
	c, err := db.OpenAux(path)
	if err != nil {
		return err
	}
	defer c.Close()
	var res string
	if err := c.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return fmt.Errorf("backup failed integrity check: %s", res)
	}
	return nil
}

func compressFile(src, dst string, key []byte) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	var sink io.Writer = out
	var crypt io.WriteCloser
	if len(key) > 0 {
		if crypt, err = NewEncryptWriter(out, key); err != nil {
			out.Close()
			return err
		}
		sink = crypt
	}
	enc, err := zstd.NewWriter(sink, zstd.WithEncoderLevel(zstd.SpeedBetterCompression), zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(4<<20), zstd.WithLowerEncoderMem(true))
	if err != nil {
		out.Close()
		return err
	}
	if _, err := io.Copy(enc, in); err != nil {
		enc.Close()
		out.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		out.Close()
		return err
	}
	if crypt != nil {
		if err := crypt.Close(); err != nil {
			out.Close()
			return err
		}
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (b *Backup) List() []BackupInfo {
	entries, _ := os.ReadDir(b.Dir)
	var out []BackupInfo
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "dials-") && (strings.HasSuffix(e.Name(), ".db.zst") || strings.HasSuffix(e.Name(), ".db.zst.enc")) {
			if info, err := e.Info(); err == nil {
				out = append(out, BackupInfo{Name: e.Name(), Bytes: info.Size(), Time: info.ModTime().Unix()})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out
}

func (b *Backup) prune() error {
	keep := b.Keep
	if keep <= 0 {
		keep = 2
	}
	for i, bi := range b.List() {
		if i >= keep {
			if err := os.Remove(filepath.Join(b.Dir, bi.Name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Restore decrypts (when needed) and decompresses a backup to dst.
func Restore(src, dst string, key []byte) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	br := bufio.NewReader(in)
	head, _ := br.Peek(len(encMagic))
	var r io.Reader = br
	if IsEncrypted(head) {
		if r, err = NewDecryptReader(br, key); err != nil {
			return err
		}
	}
	dec, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	if err != nil {
		return err
	}
	defer dec.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, dec); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
