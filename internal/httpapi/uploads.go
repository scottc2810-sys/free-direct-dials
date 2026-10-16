package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"markemind/dials/internal/importer"
	"markemind/dials/internal/platform"
)

const (
	ChunkSize = 8 << 20 // 8 MiB: well under Cloudflare's 100 MB request cap
	MaxUpload = 5 << 30 // 5 GiB per file
)

var uploadIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,40}$`)

// UploadStore keeps resumable chunked uploads on disk (TMP_DIR/upload-<id>/).
type UploadStore struct {
	Dir   string
	Clock platform.Clock
	mu    sync.Mutex
}

type UploadMeta struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	ChunkSize int64  `json:"chunk_size"`
	Chunks    int    `json:"chunks"`
	Received  []bool `json:"received"`
	Created   int64  `json:"created"`
}

func (m *UploadMeta) Missing() []int {
	var out []int
	for i, ok := range m.Received {
		if !ok {
			out = append(out, i)
		}
	}
	return out
}

func (s *UploadStore) dir(id string) (string, error) {
	if !uploadIDRe.MatchString(id) {
		return "", errors.New("bad upload id")
	}
	return filepath.Join(s.Dir, "upload-"+id), nil
}

func (s *UploadStore) Init(filename string, size int64) (*UploadMeta, error) {
	if size <= 0 || size > MaxUpload {
		return nil, fmt.Errorf("file must be between 1 byte and %d GB", MaxUpload>>30)
	}
	base := filepath.Base(strings.ReplaceAll(filename, "\\", "/"))
	if len(base) > 120 {
		base = base[:120]
	}
	m := &UploadMeta{ID: platform.Token(12), Filename: base, Size: size, ChunkSize: ChunkSize, Created: s.Clock.Now().Unix()}
	m.Chunks = int((size + ChunkSize - 1) / ChunkSize)
	m.Received = make([]bool, m.Chunks)
	d, _ := s.dir(m.ID)
	if err := os.MkdirAll(d, 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(d, "data"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return nil, err
	}
	f.Close()
	return m, s.save(m)
}

func (s *UploadStore) save(m *UploadMeta) error {
	d, err := s.dir(m.ID)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(m)
	tmp := filepath.Join(d, "meta.json.tmp")
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(d, "meta.json"))
}

func (s *UploadStore) Get(id string) (*UploadMeta, error) {
	d, err := s.dir(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(d, "meta.json"))
	if err != nil {
		return nil, errors.New("upload not found (it may have expired)")
	}
	var m UploadMeta
	return &m, json.Unmarshal(b, &m)
}

// WriteChunk stores chunk index; chunks may arrive in any order or be retried.
func (s *UploadStore) WriteChunk(id string, index int, r io.Reader) error {
	m, err := s.Get(id)
	if err != nil {
		return err
	}
	if index < 0 || index >= m.Chunks {
		return errors.New("chunk index out of range")
	}
	want := m.ChunkSize
	if index == m.Chunks-1 {
		want = m.Size - int64(index)*m.ChunkSize
	}
	d, _ := s.dir(id)
	f, err := os.OpenFile(filepath.Join(d, "data"), os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(io.NewOffsetWriter(f, int64(index)*m.ChunkSize), io.LimitReader(r, want+1))
	if err != nil {
		return err
	}
	if n != want {
		return fmt.Errorf("chunk %d has %d bytes, expected %d", index, n, want)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err = s.Get(id) // re-read under lock: parallel chunks update the same meta
	if err != nil {
		return err
	}
	m.Received[index] = true
	return s.save(m)
}

func (s *UploadStore) DataPath(id string) (string, error) {
	d, err := s.dir(id)
	return filepath.Join(d, "data"), err
}

func (s *UploadStore) Remove(id string) {
	if d, err := s.dir(id); err == nil {
		os.RemoveAll(d)
	}
}

// ImportManager runs one import at a time in the background.
type ImportManager struct {
	app *App
	mu  sync.Mutex
	cur *importJob
}

type importJob struct {
	id     int64
	stats  atomic.Pointer[importer.Stats]
	cancel context.CancelFunc
}

var ErrImportRunning = errors.New("another import is still running")

func (m *ImportManager) recoverInterrupted(ctx context.Context) error {
	_, err := m.app.DB.W.ExecContext(ctx, `UPDATE uploads SET status='failed', error='interrupted by a restart; re-run the upload (re-importing is safe and never duplicates)'
		WHERE status='running'`)
	return err
}

func (m *ImportManager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur != nil
}

// Start launches the import of a completed upload and returns the uploads row id.
func (m *ImportManager) Start(uploadID string, sheet int, mapping importer.Mapping, by string) (int64, error) {
	a := m.app
	if err := a.Guard.AllowHeavy(); err != nil {
		return 0, err
	}
	meta, err := a.Uploads.Get(uploadID)
	if err != nil {
		return 0, err
	}
	if len(meta.Missing()) > 0 {
		return 0, fmt.Errorf("upload incomplete: %d chunk(s) missing", len(meta.Missing()))
	}
	path, _ := a.Uploads.DataPath(uploadID)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur != nil {
		return 0, ErrImportRunning
	}
	now := a.Clock.Now().Unix()
	var id int64
	if err := a.DB.W.QueryRow(`INSERT INTO uploads(ts, filename, bytes, status, by) VALUES(?,?,?,'running',?) RETURNING id`,
		now, meta.Filename, meta.Size, by).Scan(&id); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &importJob{id: id, cancel: cancel}
	m.cur = job
	go m.run(ctx, job, uploadID, path, sheet, mapping, by)
	return id, nil
}

func (m *ImportManager) run(ctx context.Context, job *importJob, uploadID, path string, sheet int, mapping importer.Mapping, by string) {
	a := m.app
	defer func() {
		job.cancel()
		m.mu.Lock()
		m.cur = nil
		m.mu.Unlock()
	}()
	status, errMsg := "done", ""
	var st importer.Stats
	src, err := importer.OpenSource(path, a.Cfg.TmpDir, sheet) // CSV, TSV, gzip or .xlsx
	if err == nil {
		st, err = a.Importer.Run(ctx, src, importer.Options{Mapping: mapping, CanWrite: a.Guard.AllowHeavy, Clock: a.Clock,
			Progress: func(s importer.Stats) { job.stats.Store(&s) }})
		src.Close()
	}
	switch {
	case errors.Is(err, context.Canceled):
		status, errMsg = "cancelled", "cancelled by admin; rows imported so far are kept"
	case err != nil:
		status, errMsg = "failed", err.Error()
	}
	if status != "done" {
		// partial batches were committed: still refresh counts/caches so the site is consistent
		importer.Refresh(context.Background(), a.DB)
	}
	reasons, _ := json.Marshal(map[string]any{"skipped": st.Reasons, "cleaned": st.Cleaned, "sniffed": st.Sniffed})
	a.DB.W.Exec(`UPDATE uploads SET rows=?, added=?, updated=?, unchanged=?, skipped=?, skip_reasons=?, status=?, error=?, finished=? WHERE id=?`,
		st.Rows, st.Added, st.Updated, st.Unchanged, st.Skipped, string(reasons), status, errMsg, a.Clock.Now().Unix(), job.id)
	a.audit(context.Background(), by, "", "import."+status, fmt.Sprintf("upload #%d rows=%d added=%d updated=%d skipped=%d %s",
		job.id, st.Rows, st.Added, st.Updated, st.Skipped, errMsg))
	a.Uploads.Remove(uploadID) // the spec: delete the file when done
	a.purgeData(context.Background())
	if a.Sched != nil {
		a.Sched.RunNow(context.Background(), "seo")
	}
	if status == "failed" {
		a.Alerter.Alert(context.Background(), "import_failed", "Import failed", errMsg)
	}
}

func (m *ImportManager) Progress(id int64) (*importer.Stats, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil || m.cur.id != id {
		return nil, false
	}
	return m.cur.stats.Load(), true
}

func (m *ImportManager) Cancel(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil || m.cur.id != id {
		return false
	}
	m.cur.cancel()
	return true
}
