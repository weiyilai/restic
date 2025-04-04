package sftp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path"
	"time"

	"github.com/restic/restic/internal/backend"
	"github.com/restic/restic/internal/backend/layout"
	"github.com/restic/restic/internal/backend/limiter"
	"github.com/restic/restic/internal/backend/location"
	"github.com/restic/restic/internal/backend/util"
	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/feature"

	"github.com/cenkalti/backoff/v4"
	"github.com/pkg/sftp"
	"golang.org/x/sync/errgroup"
)

// SFTP is a backend in a directory accessed via SFTP.
type SFTP struct {
	c *sftp.Client
	p string

	cmd    *exec.Cmd
	result <-chan error

	posixRename bool

	layout.Layout
	Config
	util.Modes
}

var _ backend.Backend = &SFTP{}

var errTooShort = fmt.Errorf("file is too short")

func NewFactory() location.Factory {
	return location.NewLimitedBackendFactory("sftp", ParseConfig, location.NoPassword, limiter.WrapBackendConstructor(Create), limiter.WrapBackendConstructor(Open))
}

func startClient(cfg Config) (*SFTP, error) {
	program, args, err := buildSSHCommand(cfg)
	if err != nil {
		return nil, err
	}

	debug.Log("start client %v %v", program, args)
	// Connect to a remote host and request the sftp subsystem via the 'ssh'
	// command.  This assumes that passwordless login is correctly configured.
	cmd := exec.Command(program, args...)

	// prefix the errors with the program name
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, errors.Wrap(err, "cmd.StderrPipe")
	}

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			fmt.Fprintf(os.Stderr, "subprocess %v: %v\n", program, sc.Text())
		}
	}()

	// get stdin and stdout
	wr, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.Wrap(err, "cmd.StdinPipe")
	}
	rd, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.Wrap(err, "cmd.StdoutPipe")
	}

	bg, err := util.StartForeground(cmd)
	if err != nil {
		if errors.Is(err, exec.ErrDot) {
			return nil, errors.Errorf("cannot implicitly run relative executable %v found in current directory, use -o sftp.command=./<command> to override", cmd.Path)
		}
		return nil, err
	}

	// wait in a different goroutine
	ch := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		debug.Log("ssh command exited, err %v", err)
		for {
			ch <- errors.Wrap(err, "ssh command exited")
		}
	}()

	// open the SFTP session
	client, err := sftp.NewClientPipe(rd, wr,
		// write multiple packets (32kb) in parallel per file
		// not strictly necessary as we use ReadFromWithConcurrency
		sftp.UseConcurrentWrites(true),
		// increase send buffer per file to 4MB
		sftp.MaxConcurrentRequestsPerFile(128))
	if err != nil {
		return nil, errors.Errorf("unable to start the sftp session, error: %v", err)
	}

	err = bg()
	if err != nil {
		return nil, errors.Wrap(err, "bg")
	}

	_, posixRename := client.HasExtension("posix-rename@openssh.com")
	return &SFTP{
		c:           client,
		cmd:         cmd,
		result:      ch,
		posixRename: posixRename,
		Layout:      layout.NewDefaultLayout(cfg.Path, path.Join),
	}, nil
}

// clientError returns an error if the client has exited. Otherwise, nil is
// returned immediately.
func (r *SFTP) clientError() error {
	select {
	case err := <-r.result:
		debug.Log("client has exited with err %v", err)
		return backoff.Permanent(err)
	default:
	}

	return nil
}

// Open opens an sftp backend as described by the config by running
// "ssh" with the appropriate arguments (or cfg.Command, if set).
func Open(_ context.Context, cfg Config) (*SFTP, error) {
	debug.Log("open backend with config %#v", cfg)

	sftp, err := startClient(cfg)
	if err != nil {
		debug.Log("unable to start program: %v", err)
		return nil, err
	}

	return open(sftp, cfg)
}

func open(sftp *SFTP, cfg Config) (*SFTP, error) {
	fi, err := sftp.c.Stat(sftp.Layout.Filename(backend.Handle{Type: backend.ConfigFile}))
	m := util.DeriveModesFromFileInfo(fi, err)
	debug.Log("using (%03O file, %03O dir) permissions", m.File, m.Dir)

	sftp.Config = cfg
	sftp.p = cfg.Path
	sftp.Modes = m
	return sftp, nil
}

func (r *SFTP) mkdirAllDataSubdirs(ctx context.Context, nconn uint) error {
	// Run multiple MkdirAll calls concurrently. These involve multiple
	// round-trips and we do a lot of them, so this whole operation can be slow
	// on high-latency links.
	g, _ := errgroup.WithContext(ctx)
	// Use errgroup's built-in semaphore, because r.sem is not initialized yet.
	g.SetLimit(int(nconn))

	for _, d := range r.Paths() {
		d := d
		g.Go(func() error {
			// First try Mkdir. For most directories in Paths, this takes one
			// round trip, not counting duplicate parent creations causes by
			// concurrency. MkdirAll first does Stat, then recursive MkdirAll
			// on the parent, so calls typically take three round trips.
			if err := r.c.Mkdir(d); err == nil {
				return nil
			}
			return errors.Wrapf(r.c.MkdirAll(d), "MkdirAll %v", d)
		})
	}

	return g.Wait()
}

// IsNotExist returns true if the error is caused by a not existing file.
func (r *SFTP) IsNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}

func (r *SFTP) IsPermanentError(err error) bool {
	return r.IsNotExist(err) || errors.Is(err, errTooShort) || errors.Is(err, os.ErrPermission)
}

func buildSSHCommand(cfg Config) (cmd string, args []string, err error) {
	if cfg.Command != "" {
		args, err := backend.SplitShellStrings(cfg.Command)
		if err != nil {
			return "", nil, err
		}
		if cfg.Args != "" {
			return "", nil, errors.New("cannot specify both sftp.command and sftp.args options")
		}

		return args[0], args[1:], nil
	}

	cmd = "ssh"

	host, port := cfg.Host, cfg.Port

	args = []string{host}
	if port != "" {
		args = append(args, "-p", port)
	}
	if cfg.User != "" {
		args = append(args, "-l", cfg.User)
	}

	if cfg.Args != "" {
		a, err := backend.SplitShellStrings(cfg.Args)
		if err != nil {
			return "", nil, err
		}

		args = append(args, a...)
	}

	args = append(args, "-s", "sftp")
	return cmd, args, nil
}

// Create creates an sftp backend as described by the config by running "ssh"
// with the appropriate arguments (or cfg.Command, if set).
func Create(ctx context.Context, cfg Config) (*SFTP, error) {
	sftp, err := startClient(cfg)
	if err != nil {
		debug.Log("unable to start program: %v", err)
		return nil, err
	}

	sftp.Modes = util.DefaultModes

	// test if config file already exists
	_, err = sftp.c.Lstat(sftp.Layout.Filename(backend.Handle{Type: backend.ConfigFile}))
	if err == nil {
		return nil, errors.New("config file already exists")
	}

	// create paths for data and refs
	if err = sftp.mkdirAllDataSubdirs(ctx, cfg.Connections); err != nil {
		return nil, err
	}

	// repurpose existing connection
	return open(sftp, cfg)
}

func (r *SFTP) Properties() backend.Properties {
	return backend.Properties{
		Connections:      r.Config.Connections,
		HasAtomicReplace: r.posixRename,
	}
}

// Hasher may return a hash function for calculating a content hash for the backend
func (r *SFTP) Hasher() hash.Hash {
	return nil
}

// tempSuffix generates a random string suffix that should be sufficiently long
// to avoid accidental conflicts
func tempSuffix() string {
	var nonce [16]byte
	_, err := rand.Read(nonce[:])
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(nonce[:])
}

// Save stores data in the backend at the handle.
func (r *SFTP) Save(_ context.Context, h backend.Handle, rd backend.RewindReader) error {
	if err := r.clientError(); err != nil {
		return err
	}

	filename := r.Filename(h)
	tmpFilename := filename + "-restic-temp-" + tempSuffix()
	dirname := r.Dirname(h)

	// create new file
	f, err := r.c.OpenFile(tmpFilename, os.O_CREATE|os.O_EXCL|os.O_WRONLY)

	if r.IsNotExist(err) {
		// error is caused by a missing directory, try to create it
		mkdirErr := r.c.MkdirAll(r.Dirname(h))
		if mkdirErr != nil {
			debug.Log("error creating dir %v: %v", r.Dirname(h), mkdirErr)
		} else {
			// try again
			f, err = r.c.OpenFile(tmpFilename, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
		}
	}

	if err != nil {
		return errors.Wrapf(err, "OpenFile %v", tmpFilename)
	}

	// pkg/sftp doesn't allow creating with a mode.
	// Chmod while the file is still empty.
	if err == nil {
		err = f.Chmod(r.Modes.File)
		if err != nil {
			return errors.Wrapf(err, "Chmod %v", tmpFilename)
		}
	}

	defer func() {
		if err == nil {
			return
		}

		// Try not to leave a partial file behind.
		rmErr := r.c.Remove(f.Name())
		if rmErr != nil {
			debug.Log("sftp: failed to remove broken file %v: %v",
				f.Name(), rmErr)
		}
	}()

	// save data, make sure to use the optimized sftp upload method
	wbytes, err := f.ReadFromWithConcurrency(rd, 0)
	if err != nil {
		_ = f.Close()
		err = r.checkNoSpace(dirname, rd.Length(), err)
		return errors.Wrapf(err, "Write %v", tmpFilename)
	}

	// sanity check
	if wbytes != rd.Length() {
		_ = f.Close()
		return errors.Errorf("Write %v: wrote %d bytes instead of the expected %d bytes", tmpFilename, wbytes, rd.Length())
	}

	err = f.Close()
	if err != nil {
		return errors.Wrapf(err, "Close %v", tmpFilename)
	}

	// Prefer POSIX atomic rename if available.
	if r.posixRename {
		err = r.c.PosixRename(tmpFilename, filename)
	} else {
		err = r.c.Rename(tmpFilename, filename)
	}
	return errors.Wrapf(err, "Rename %v", tmpFilename)
}

// checkNoSpace checks if err was likely caused by lack of available space
// on the remote, and if so, makes it permanent.
func (r *SFTP) checkNoSpace(dir string, size int64, origErr error) error {
	// The SFTP protocol has a message for ENOSPC,
	// but pkg/sftp doesn't export it and OpenSSH's sftp-server
	// sends FX_FAILURE instead.

	e, ok := origErr.(*sftp.StatusError)
	_, hasExt := r.c.HasExtension("statvfs@openssh.com")
	if !ok || e.FxCode() != sftp.ErrSSHFxFailure || !hasExt {
		return origErr
	}

	fsinfo, err := r.c.StatVFS(dir)
	if err != nil {
		debug.Log("sftp: StatVFS returned %v", err)
		return origErr
	}
	if fsinfo.Favail == 0 || fsinfo.Frsize*fsinfo.Bavail < uint64(size) {
		err := errors.New("sftp: no space left on device")
		return backoff.Permanent(err)
	}
	return origErr
}

// Load runs fn with a reader that yields the contents of the file at h at the
// given offset.
func (r *SFTP) Load(ctx context.Context, h backend.Handle, length int, offset int64, fn func(rd io.Reader) error) error {
	if err := r.clientError(); err != nil {
		return err
	}

	return util.DefaultLoad(ctx, h, length, offset, r.openReader, func(rd io.Reader) error {
		if length == 0 || !feature.Flag.Enabled(feature.BackendErrorRedesign) {
			return fn(rd)
		}

		// there is no direct way to efficiently check whether the file is too short
		// rd is already a LimitedReader which can be used to track the number of bytes read
		err := fn(rd)

		// check the underlying reader to be agnostic to however fn() handles the returned error
		_, rderr := rd.Read([]byte{0})
		if rderr == io.EOF && rd.(*util.LimitedReadCloser).N != 0 {
			// file is too short
			return fmt.Errorf("%w: %v", errTooShort, err)
		}

		return err
	})
}

func (r *SFTP) openReader(_ context.Context, h backend.Handle, length int, offset int64) (io.ReadCloser, error) {
	f, err := r.c.Open(r.Filename(h))
	if err != nil {
		return nil, errors.Wrapf(err, "Open %v", r.Filename(h))
	}

	if offset > 0 {
		_, err = f.Seek(offset, 0)
		if err != nil {
			_ = f.Close()
			return nil, errors.Wrapf(err, "Seek %v", r.Filename(h))
		}
	}

	if length > 0 {
		// unlimited reads usually use io.Copy which needs WriteTo support at the underlying reader
		// limited reads are usually combined with io.ReadFull which reads all required bytes into a buffer in one go
		return util.LimitReadCloser(f, int64(length)), nil
	}

	return f, nil
}

// Stat returns information about a blob.
func (r *SFTP) Stat(_ context.Context, h backend.Handle) (backend.FileInfo, error) {
	if err := r.clientError(); err != nil {
		return backend.FileInfo{}, err
	}

	fi, err := r.c.Lstat(r.Filename(h))
	if err != nil {
		return backend.FileInfo{}, errors.Wrapf(err, "Lstat %v", r.Filename(h))
	}

	return backend.FileInfo{Size: fi.Size(), Name: h.Name}, nil
}

// Remove removes the content stored at name.
func (r *SFTP) Remove(_ context.Context, h backend.Handle) error {
	if err := r.clientError(); err != nil {
		return err
	}

	return errors.Wrapf(r.c.Remove(r.Filename(h)), "Remove %v", r.Filename(h))
}

// List runs fn for each file in the backend which has the type t. When an
// error occurs (or fn returns an error), List stops and returns it.
func (r *SFTP) List(ctx context.Context, t backend.FileType, fn func(backend.FileInfo) error) error {
	if err := r.clientError(); err != nil {
		return err
	}

	basedir, subdirs := r.Basedir(t)
	walker := r.c.Walk(basedir)
	for {
		ok := walker.Step()
		if !ok {
			break
		}

		if walker.Err() != nil {
			if r.IsNotExist(walker.Err()) {
				debug.Log("ignoring non-existing directory")
				return nil
			}
			return errors.Wrapf(walker.Err(), "Walk %v", basedir)
		}

		if walker.Path() == basedir {
			continue
		}

		if walker.Stat().IsDir() && !subdirs {
			walker.SkipDir()
			continue
		}

		fi := walker.Stat()
		if !fi.Mode().IsRegular() {
			continue
		}

		debug.Log("send %v\n", path.Base(walker.Path()))

		rfi := backend.FileInfo{
			Name: path.Base(walker.Path()),
			Size: fi.Size(),
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := fn(rfi)
		if err != nil {
			return err
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	return ctx.Err()
}

var closeTimeout = 2 * time.Second

// Close closes the sftp connection and terminates the underlying command.
func (r *SFTP) Close() error {
	if r == nil {
		return nil
	}

	err := errors.Wrap(r.c.Close(), "Close")
	debug.Log("Close returned error %v", err)

	// wait for closeTimeout before killing the process
	select {
	case err := <-r.result:
		return err
	case <-time.After(closeTimeout):
	}

	if err := r.cmd.Process.Kill(); err != nil {
		return err
	}

	// get the error, but ignore it
	<-r.result
	return nil
}

func (r *SFTP) deleteRecursive(ctx context.Context, name string) error {
	entries, err := r.c.ReadDir(name)
	if err != nil {
		return errors.Wrapf(err, "ReadDir %v", name)
	}

	for _, fi := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		itemName := path.Join(name, fi.Name())
		if fi.IsDir() {
			err := r.deleteRecursive(ctx, itemName)
			if err != nil {
				return err
			}

			err = r.c.RemoveDirectory(itemName)
			if err != nil {
				return errors.Wrapf(err, "RemoveDirectory %v", itemName)
			}

			continue
		}

		err := r.c.Remove(itemName)
		if err != nil {
			return errors.Wrapf(err, "Remove %v", itemName)
		}
	}

	return nil
}

// Delete removes all data in the backend.
func (r *SFTP) Delete(ctx context.Context) error {
	return r.deleteRecursive(ctx, r.p)
}

// Warmup not implemented
func (r *SFTP) Warmup(_ context.Context, _ []backend.Handle) ([]backend.Handle, error) {
	return []backend.Handle{}, nil
}
func (r *SFTP) WarmupWait(_ context.Context, _ []backend.Handle) error { return nil }
