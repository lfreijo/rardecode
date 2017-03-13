package rardecode

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"os"
	"time"
)

// FileHeader HostOS types
const (
	HostOSUnknown = 0
	HostOSMSDOS   = 1
	HostOSOS2     = 2
	HostOSWindows = 3
	HostOSUnix    = 4
	HostOSMacOS   = 5
	HostOSBeOS    = 6
)

const (
	maxPassword = int(128)

	_ = iota
	decode29Ver
	decode50Ver
)

var (
	errShortFile        = errors.New("rardecode: decoded file too short")
	errInvalidFileBlock = errors.New("rardecode: invalid file block")
	errUnexpectedArcEnd = errors.New("rardecode: unexpected end of archive")
	errBadFileChecksum  = errors.New("rardecode: bad file checksum")
	errSolidOpen        = errors.New("rardecode: solid files don't support Open")
	errUnknownArc       = errors.New("rardecode: unknown archive version")
)

type byteReader interface {
	io.Reader
	bytes() ([]byte, error)
}

type limitedReader struct {
	r        byteReader
	n        int64 // bytes remaining
	shortErr error // error returned when r returns io.EOF with n > 0
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > l.n {
		p = p[0:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	if err == io.EOF && l.n > 0 {
		return n, l.shortErr
	}
	return n, err
}

func (l *limitedReader) bytes() ([]byte, error) {
	b, err := l.r.bytes()
	if n := len(b); int64(n) > l.n {
		b = b[:int(l.n)]
	}
	l.n -= int64(len(b))
	return b, err
}

type limitedByteReader struct {
	n int64
	v *volume
}

func (l *limitedByteReader) init() error { return l.v.init() }

func (l *limitedByteReader) Close() error { return l.v.Close() }

// Read reads from v and stops with io.EOF after n bytes.
// If v returns an io.EOF before reading n bytes, io.ErrUnexpectedEOF is returned.
func (l *limitedByteReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > l.n {
		p = p[0:l.n]
	}
	n, err := l.v.Read(p)
	l.n -= int64(n)
	if err == io.EOF && l.n > 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func (l *limitedByteReader) skip() error {
	if l.n == 0 {
		return nil
	}
	n := l.n
	l.n = 0
	return l.v.discard(n)
}

// blocks returns a byte slice whose size is a multiple of blockSize.
// If there is less than blockSize bytes available before EOF, then those
// bytes will be returned.
func (l *limitedByteReader) blocks(blockSize int) ([]byte, error) {
	if l.n == 0 {
		return nil, io.EOF
	}
	var n int
	if l.n < int64(blockSize) {
		n = int(l.n)
	} else {
		n = maxInt
		if l.n < int64(n) {
			n = int(l.n)
		}
		b, err := l.v.peek(n)
		if err != nil && err != bufio.ErrBufferFull {
			return nil, err
		}
		n = len(b)
		n -= n % blockSize
	}
	b, err := l.v.readSlice(n)
	l.n -= int64(len(b))
	return b, err
}

// FileHeader represents a single file in a RAR archive.
type FileHeader struct {
	Name             string    // file name using '/' as the directory separator
	IsDir            bool      // is a directory
	Solid            bool      // is a solid file
	HostOS           byte      // Host OS the archive was created on
	Attributes       int64     // Host OS specific file attributes
	PackedSize       int64     // packed file size (or first block if the file spans volumes)
	UnPackedSize     int64     // unpacked file size
	UnKnownSize      bool      // unpacked file size is not known
	ModificationTime time.Time // modification time (non-zero if set)
	CreationTime     time.Time // creation time (non-zero if set)
	AccessTime       time.Time // access time (non-zero if set)
	Version          int       // file version
}

// Mode returns an os.FileMode for the file, calculated from the Attributes field.
func (f *FileHeader) Mode() os.FileMode {
	var m os.FileMode

	if f.IsDir {
		m = os.ModeDir
	}
	if f.HostOS == HostOSWindows {
		if f.IsDir {
			m |= 0777
		} else if f.Attributes&1 > 0 {
			m |= 0444 // readonly
		} else {
			m |= 0666
		}
		return m
	}
	// assume unix perms for all remaining os types
	m |= os.FileMode(f.Attributes) & os.ModePerm

	// only check other bits on unix host created archives
	if f.HostOS != HostOSUnix {
		return m
	}

	if f.Attributes&0x200 != 0 {
		m |= os.ModeSticky
	}
	if f.Attributes&0x400 != 0 {
		m |= os.ModeSetgid
	}
	if f.Attributes&0x800 != 0 {
		m |= os.ModeSetuid
	}

	// Check for additional file types.
	if f.Attributes&0xF000 == 0xA000 {
		m |= os.ModeSymlink
	}
	return m
}

// fileBlockHeader represents a file block in a RAR archive.
// Files may comprise one or more file blocks.
// Solid files retain decode tables and dictionary from previous solid files in the archive.
type fileBlockHeader struct {
	first    bool      // first block in file
	last     bool      // last block in file
	arcSolid bool      // archive is solid
	winSize  uint      // log base 2 of decode window size
	hash     hash.Hash // hash used for file checksum
	hashKey  []byte    // optional hmac key to be used calculate file checksum
	sum      []byte    // expected checksum for file contents
	decVer   int       // decoder to use for file
	key      []byte    // key for AES, non-empty if file encrypted
	iv       []byte    // iv for AES, non-empty if file encrypted
	FileHeader
}

// fileBlockReader provides sequential access to file blocks in a RAR archive.
type fileBlockReader interface {
	io.Reader                             // provides read access to current file block data
	io.Closer                             // closes volume file opened by fileBlockReader
	blocks(blockSize int) ([]byte, error) // returns a byte slice in multiples of blockSize from current block
	next() (*fileBlockHeader, error)      // advances to the next file block
	clone() fileBlockReader               // makes a copy of the fileBlockReader
	init() error                          // initializes a cloned fileBlockReader
}

// packedFileReader provides sequential access to packed files in a RAR archive.
type packedFileReader struct {
	h *fileBlockHeader // current file header
	r fileBlockReader
}

// nextBlock reads the next file block in the current file at the current
// archive file position, or returns an error if there is a problem.
// It is invalid to call this when already at the last block in the current file.
func (f *packedFileReader) nextBlock() error {
	if f.h == nil || f.h.last {
		return io.EOF
	}
	h, err := f.r.next()
	if err != nil {
		if err == io.EOF {
			// archive ended, but file hasn't
			return errUnexpectedArcEnd
		}
		return err
	}
	if h.first || h.Name != f.h.Name {
		return errInvalidFileBlock
	}
	f.h = h
	return nil
}

// next advances to the next packed file in the RAR archive.
func (f *packedFileReader) next() (*fileBlockHeader, error) {
	// skip to last block in current file
	var err error
	for err == nil {
		err = f.nextBlock()
	}
	if err != io.EOF {
		return nil, err
	}
	f.h, err = f.r.next() // get next file block
	if err != nil {
		return nil, err
	}
	if !f.h.first {
		return nil, errInvalidFileBlock
	}
	return f.h, nil
}

// Read reads the packed data for the current file into p.
func (f *packedFileReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p) // read current block data
	for err == io.EOF {   // current block empty
		if n > 0 {
			return n, nil
		}
		if err = f.nextBlock(); err != nil {
			return 0, err
		}
		n, err = f.r.Read(p) // read new block data
	}
	return n, err
}

// blocks returns a byte slice whose size is always a multiple of blockSize.
func (f *packedFileReader) blocks(blockSize int) ([]byte, error) {
	b, err := f.r.blocks(blockSize)
	for err == io.EOF {
		if err = f.nextBlock(); err != nil {
			return nil, err
		}
		b, err = f.r.blocks(blockSize) // read new block data
	}
	if len(b) >= blockSize || err != nil {
		return b, err
	}

	// slice returned is smaller than blockSize. Try to get the rest
	// from the following file blocks.
	buf := make([]byte, blockSize)
	n := copy(buf, b)
	err = f.nextBlock()
	for err == nil {
		var nn int
		// read a single small block of the remaining bytes
		nn, err = io.ReadFull(f.r, buf[n:])
		switch err {
		case nil:
			return buf, nil
		case io.EOF, io.ErrUnexpectedEOF:
			err = f.nextBlock()
		}
		n += nn
	}
	return nil, err
}

func (f *packedFileReader) bytes() ([]byte, error) { return f.blocks(1) }

func newPackedFileReader(v *volume, pass string) (*packedFileReader, error) {
	runes := []rune(pass)
	if len(runes) > maxPassword {
		pass = string(runes[:maxPassword])
	}
	if err := v.init(); err != nil {
		return nil, err
	}
	if err := v.findSig(); err != nil {
		_ = v.Close()
		return nil, err
	}
	var fbr fileBlockReader
	switch v.ver {
	case fileFmt15:
		fbr = newArchive15(v, pass)
	case fileFmt50:
		fbr = newArchive50(v, pass)
	default:
		return nil, errUnknownArc
	}
	return &packedFileReader{r: fbr}, nil
}

type checksumReader struct {
	r    byteReader
	hash hash.Hash
	pr   *packedFileReader
}

func (cr *checksumReader) eofError() error {
	// calculate file checksum
	h := cr.pr.h
	sum := cr.hash.Sum(nil)
	if len(h.hashKey) > 0 {
		mac := hmac.New(sha256.New, h.hashKey)
		_, _ = mac.Write(sum) // ignore error, should always succeed
		sum = mac.Sum(sum[:0])
		if len(h.sum) == 4 {
			// CRC32
			for i, v := range sum[4:] {
				sum[i&3] ^= v
			}
			sum = sum[:4]
		}
	}
	if !bytes.Equal(sum, h.sum) {
		return errBadFileChecksum
	}
	return io.EOF
}

func (cr *checksumReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 {
		if n, err = cr.hash.Write(p[:n]); err != nil {
			return n, err
		}
	}
	if err != io.EOF {
		return n, err
	}
	return n, cr.eofError()
}

func (cr *checksumReader) bytes() ([]byte, error) {
	b, err := cr.r.bytes()
	if len(b) > 0 {
		if _, err = cr.hash.Write(b); err != nil {
			return b, err
		}
	}
	if err != io.EOF {
		return b, err
	}
	return b, cr.eofError()
}

// Reader provides sequential access to files in a RAR archive.
type Reader struct {
	r  byteReader        // reader for current unpacked file
	dr *decodeReader     // reader for decoding and filters if file is compressed
	pr *packedFileReader // reader for current raw file bytes
}

// Read reads from the current file in the RAR archive.
func (r *Reader) Read(p []byte) (int, error) {
	if r.r == nil {
		err := r.nextFile()
		if err != nil {
			return 0, err
		}
	}
	return r.r.Read(p)
}

// WriteTo implements io.WriterTo.
func (r *Reader) WriteTo(w io.Writer) (int64, error) {
	if r.r == nil {
		err := r.nextFile()
		if err != nil {
			return 0, err
		}
	}
	var n int64
	b, err := r.r.bytes()
	for err == nil {
		var nn int
		nn, err = w.Write(b)
		n += int64(nn)
		if err == nil {
			b, err = r.r.bytes()
		}
	}
	if err == io.EOF {
		err = nil
	}
	return n, err
}

// Next advances to the next file in the archive.
func (r *Reader) Next() (*FileHeader, error) {
	// check if file is a compressed file in a solid archive
	if h := r.pr.h; h != nil && h.decVer > 0 && h.arcSolid {
		var err error
		if r.r == nil {
			// setup full file reader
			err = r.nextFile()
		}
		// decode and discard bytes
		for err == nil {
			_, err = r.dr.bytes()
		}
		if err != io.EOF {
			return nil, err
		}
	}
	// get next packed file
	h, err := r.pr.next()
	if err != nil {
		return nil, err
	}
	// Clear the reader as it will be setup on the next Read() or WriteTo().
	r.r = nil
	return &h.FileHeader, nil
}

func (r *Reader) nextFile() error {
	h := r.pr.h
	if h == nil {
		return io.EOF
	}
	// start with packed file reader
	r.r = r.pr
	// check for encryption
	if len(h.key) > 0 && len(h.iv) > 0 {
		r.r = newAesDecryptReader(r.pr, h.key, h.iv) // decrypt
	}
	// check for compression
	if h.decVer > 0 {
		if r.dr == nil {
			r.dr = new(decodeReader)
		}
		err := r.dr.init(r.r, h.decVer, h.winSize, !h.Solid, h.arcSolid)
		if err != nil {
			return err
		}
		r.r = r.dr
	}
	if h.UnPackedSize >= 0 && !h.UnKnownSize {
		// Limit reading to UnPackedSize as there may be padding
		r.r = &limitedReader{r.r, h.UnPackedSize, errShortFile}
	}
	if h.hash != nil {
		r.r = &checksumReader{r.r, h.hash, r.pr}
	}
	return nil
}

// NewReader creates a Reader reading from r.
// NewReader only supports single volume archives.
// Multi-volume archives must use OpenReader.
func NewReader(r io.Reader, password string) (*Reader, error) {
	pr, err := newPackedFileReader(&volume{f: r}, password)
	if err != nil {
		return nil, err
	}
	return &Reader{pr: pr}, nil
}

// ReadCloser is a Reader that allows closing of the rar archive.
type ReadCloser struct {
	Reader
}

// Close closes the rar file.
func (rc *ReadCloser) Close() error {
	return rc.pr.r.Close()
}

// OpenReader opens a RAR archive specified by the name and returns a ReadCloser.
func OpenReader(name, password string) (*ReadCloser, error) {
	pr, err := newPackedFileReader(&volume{name: name}, password)
	if err != nil {
		return nil, err
	}
	return &ReadCloser{Reader{pr: pr}}, nil
}

// File represents a file in a RAR archive
type File struct {
	FileHeader
	h *fileBlockHeader
	r fileBlockReader
}

// Open returns an io.ReadCloser that provides access to the File's contents.
// Open is not supported on Solid File's as their contents depend on the decoding
// of the preceding files in the archive. Use OpenReader and Next to access Solid file
// contents instead.
func (f *File) Open() (io.ReadCloser, error) {
	if f.Solid {
		return nil, errSolidOpen
	}
	err := f.r.init()
	if err != nil {
		return nil, err
	}
	r := new(ReadCloser)
	r.pr = &packedFileReader{r: f.r, h: f.h}
	return r, nil
}

// List returns a list of File's in the RAR archive specified by name.
func List(name, password string) ([]*File, error) {
	r, err := OpenReader(name, password)
	if err != nil {
		return nil, err
	}
	pr := r.pr
	defer pr.r.Close()

	var fl []*File
	for {
		// get next file
		h, err := pr.next()
		if err != nil {
			if err == io.EOF {
				return fl, nil
			}
			return nil, err
		}

		// save information for File
		f := new(File)
		f.FileHeader = h.FileHeader
		f.h = h
		f.r = pr.r.clone()
		fl = append(fl, f)
	}
}
