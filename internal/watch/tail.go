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
	ft := &FileTailer{path: path, file: f, debugLog: newDebugRateLimiter(2)}

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

// ReadLines reads any new lines appended since last call.
// Returns lines without trailing newline. Handles rotation:
// if the file inode changes or the size shrinks (truncation), reopen from start.
func (ft *FileTailer) ReadLines() ([]string, error) {
	// Check for rotation via stat on the path.
	info, err := os.Stat(ft.path)
	if err != nil {
		// File disappeared; wait for it to come back.
		return nil, nil
	}

	var curInode uint64
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		curInode = st.Ino
	}

	rotated := (curInode != 0 && curInode != ft.inode) || info.Size() < ft.offset

	if rotated {
		ft.file.Close()
		f, err := os.Open(ft.path)
		if err != nil {
			return nil, nil
		}
		ft.file = f
		ft.offset = 0
		ft.inode = curInode
	}

	_, err = ft.file.Seek(ft.offset, io.SeekStart)
	if err != nil {
		return nil, err
	}

	reader := bufio.NewReader(ft.file)
	var lines []string
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			// Only emit complete lines (ending with newline).
			if len(line) > 0 && line[len(line)-1] == '\n' {
				lines = append(lines, line[:len(line)-1])
				ft.offset += int64(len(line))
			} else {
				// Partial line — don't advance offset, wait for rest.
				break
			}
		}
		if err != nil {
			break
		}
	}
	return lines, nil
}

// Close closes the underlying file.
func (ft *FileTailer) Close() error {
	return ft.file.Close()
}
