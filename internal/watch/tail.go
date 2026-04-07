package watch

import (
	"bufio"
	"io"
	"os"
	"syscall"
)

// FileTailer tails a single file, tracking inode and offset for rotation detection.
type FileTailer struct {
	path     string
	file     *os.File
	reader   *bufio.Reader // reused across ReadLines calls to avoid per-call allocation
	offset   int64
	inode    uint64
	debugLog *debugRateLimiter
}

// NewFileTailer creates a tailer; if readFromEnd is true, starts at EOF.
func NewFileTailer(path string, readFromEnd bool) (*FileTailer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	ft := &FileTailer{path: path, file: f, reader: bufio.NewReader(f), debugLog: newDebugRateLimiter(2)}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		ft.inode = st.Ino
	}

	if readFromEnd {
		ft.offset, err = f.Seek(0, io.SeekEnd)
		if err != nil {
			f.Close()
			return nil, err
		}
	}
	return ft, nil
}

// ReadLines reads any new complete lines appended since the last call.
// Returns lines without the trailing newline. Partial lines (no trailing '\n')
// are left in the bufio buffer and returned on the next call.
// Rotation detection and reopening is handled externally via CheckRotation.
func (ft *FileTailer) ReadLines() ([]string, error) {
	var lines []string
	for {
		line, err := ft.reader.ReadString('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			lines = append(lines, line[:len(line)-1])
			ft.offset += int64(len(line))
		} else {
			break // partial line — stays in bufio buffer for next call
		}
		if err != nil {
			break
		}
	}
	return lines, nil
}

// Reopen closes the current file descriptor and reopens the file at ft.path.
// If readFromEnd is true, seeks to EOF; otherwise starts at the beginning.
// Used for rotation handling (called after a CREATE event or by CheckRotation).
func (ft *FileTailer) Reopen(readFromEnd bool) error {
	ft.file.Close()
	f, err := os.Open(ft.path)
	if err != nil {
		return err
	}
	ft.file = f
	ft.reader.Reset(f)
	ft.offset = 0
	if readFromEnd {
		off, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			f.Close()
			return err
		}
		ft.offset = off
	}
	if info, err := f.Stat(); err == nil {
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			ft.inode = st.Ino
		}
	}
	return nil
}

// CheckRotation checks whether the file has been rotated (inode changed or
// size shrank). If so, it reopens from the beginning and returns (true, nil).
// If the file has disappeared it returns (false, nil) — the caller handles
// re-discovery. Returns (false, nil) if no rotation was detected.
func (ft *FileTailer) CheckRotation() (bool, error) {
	info, err := os.Stat(ft.path)
	if err != nil {
		return false, nil // file disappeared; caller handles
	}
	var curInode uint64
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		curInode = st.Ino
	}
	rotated := (curInode != 0 && curInode != ft.inode) || info.Size() < ft.offset
	if rotated {
		return true, ft.Reopen(false)
	}
	return false, nil
}

// Close closes the underlying file.
func (ft *FileTailer) Close() error {
	return ft.file.Close()
}
