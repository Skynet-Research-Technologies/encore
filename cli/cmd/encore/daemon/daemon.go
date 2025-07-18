package daemon

import (
	"context"
	"database/sql"
	"embed"
	_ "embed" // for go:embed
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/cockroachdb/errors"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/mattn/go-sqlite3" // for "sqlite3" driver
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"encr.dev/cli/daemon"
	"encr.dev/cli/daemon/apps"
	"encr.dev/cli/daemon/dash"
	"encr.dev/cli/daemon/engine"
	"encr.dev/cli/daemon/engine/trace2"
	"encr.dev/cli/daemon/engine/trace2/sqlite"
	"encr.dev/cli/daemon/mcp"
	"encr.dev/cli/daemon/namespace"
	"encr.dev/cli/daemon/objects"
	"encr.dev/cli/daemon/run"
	"encr.dev/cli/daemon/secret"
	"encr.dev/cli/daemon/sqldb"
	"encr.dev/cli/daemon/sqldb/docker"
	"encr.dev/cli/daemon/sqldb/external"
	"encr.dev/internal/conf"
	"encr.dev/internal/env"
	"encr.dev/pkg/eerror"
	"encr.dev/pkg/option"
	"encr.dev/pkg/watcher"
	"encr.dev/pkg/xos"
	daemonpb "encr.dev/proto/encore/daemon"
)

// Main runs the daemon.
func Main() {
	watcher.BumpRLimitSoftToHardLimit()

	if err := redirectLogOutput(); err != nil {
		log.Error().Err(err).Msg("could not setup daemon log file, skipping")
	}
	if err := runMain(); err != nil {
		log.Fatal().Err(err).Msg("daemon failed")
	}
}

func runMain() (err error) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT)
	defer cancel()

	// exit receives signals from the different subsystems
	// that something went wrong and it's time to exit.
	// Sending nil indicates it's time to gracefully exit.
	exit := make(chan error)

	d := &Daemon{dev: conf.DevDaemon, exit: exit}
	defer handleBailout(&err)
	defer d.closeAll()

	d.init(ctx)
	d.serve()

	select {
	case err := <-exit:
		return err
	case <-ctx.Done():
		return nil
	}
}

// Daemon orchestrates setting up the different daemon subsystems.
type Daemon struct {
	Daemon        *net.UnixListener
	Runtime       *retryingTCPListener
	DBProxy       *retryingTCPListener
	Dash          *retryingTCPListener
	Debug         *retryingTCPListener
	ObjectStorage *retryingTCPListener
	MCP           *retryingTCPListener
	EncoreDB      *sql.DB

	Apps          *apps.Manager
	Secret        *secret.Manager
	RunMgr        *run.Manager
	NS            *namespace.Manager
	ClusterMgr    *sqldb.ClusterManager
	ObjectsMgr    *objects.ClusterManager
	MCPMgr        *mcp.Manager
	PublicBuckets *objects.PublicBucketServer
	Trace         trace2.Store
	Server        *daemon.Server
	dev           bool // whether we're in development mode

	// exit is a channel that shuts down the daemon when sent on.
	// A nil error indicates graceful exit.
	exit chan<- error

	// close are the things to close when exiting.
	close []io.Closer
}

func (d *Daemon) init(ctx context.Context) {
	d.Daemon = d.listenDaemonSocket()
	d.Dash = d.listenTCPRetry("dashboard", env.EncoreDevDashListenAddr(), 9400)
	d.DBProxy = d.listenTCPRetry("dbproxy", option.None[string](), 9500)
	d.Runtime = d.listenTCPRetry("runtime", option.None[string](), 9600)
	d.Debug = d.listenTCPRetry("debug", option.None[string](), 9700)
	d.ObjectStorage = d.listenTCPRetry("objectstorage", env.EncoreObjectStorageListAddr(), 9800)
	d.MCP = d.listenTCPRetry("mcp", env.EncoreMCPSSEListenAddr(), 9900)
	d.EncoreDB = d.openDB()

	d.Apps = apps.NewManager(d.EncoreDB)
	d.close = append(d.close, d.Apps)

	// If ENCORE_SQLDB_HOST is set, use the external cluster instead of
	// creating our own docker container cluster.
	var sqldbDriver sqldb.Driver = &docker.Driver{}
	if host := os.Getenv("ENCORE_SQLDB_HOST"); host != "" {
		sqldbDriver = &external.Driver{
			Host:              host,
			Database:          os.Getenv("ENCORE_SQLDB_DATABASE"),
			SuperuserUsername: os.Getenv("ENCORE_SQLDB_USER"),
			SuperuserPassword: os.Getenv("ENCORE_SQLDB_PASSWORD"),
		}
		log.Info().Msgf("using external postgres cluster: %s", host)
	}

	d.NS = namespace.NewManager(d.EncoreDB)
	d.Secret = secret.New()
	d.ClusterMgr = sqldb.NewClusterManager(sqldbDriver, d.Apps, d.NS, d.Secret)
	d.ObjectsMgr = objects.NewClusterManager(d.NS)
	d.PublicBuckets = objects.NewPublicBucketServer("http://"+d.ObjectStorage.ClientAddr(), d.ObjectsMgr.PersistentStoreFallback)

	d.Trace = sqlite.New(ctx, d.EncoreDB)
	d.RunMgr = &run.Manager{
		RuntimePort:   d.Runtime.Port(),
		DBProxyPort:   d.DBProxy.Port(),
		DashBaseURL:   fmt.Sprintf("http://%s", d.Dash.ClientAddr()),
		Secret:        d.Secret,
		ClusterMgr:    d.ClusterMgr,
		ObjectsMgr:    d.ObjectsMgr,
		PublicBuckets: d.PublicBuckets,
	}
	d.MCPMgr = mcp.NewManager(
		d.Apps,
		d.ClusterMgr,
		d.NS,
		d.Trace,
		d.RunMgr,
		fmt.Sprintf("http://%s", d.MCP.ClientAddr()),
	)

	// Register namespace deletion handlers.
	d.NS.RegisterDeletionHandler(d.ClusterMgr)
	d.NS.RegisterDeletionHandler(d.RunMgr)
	d.NS.RegisterDeletionHandler(d.ObjectsMgr)

	d.Server = daemon.New(d.Apps, d.RunMgr, d.ClusterMgr, d.Secret, d.NS, d.MCPMgr)
}

func (d *Daemon) serve() {
	go d.serveDaemon()
	go d.serveRuntime()
	go d.serveDBProxy()
	go d.serveDash()
	go d.serveDebug()
	go d.serveObjects()
	go d.serveMCP()
}

// listenDaemonSocket listens on the encored.sock UNIX socket
// and arranges to exit when the socket is closed.
func (d *Daemon) listenDaemonSocket() *net.UnixListener {
	userCacheDir, err := os.UserCacheDir()
	if err != nil {
		fatal(err)
	}
	socketPath := filepath.Join(userCacheDir, "encore", "encored.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		fatal(err)
	}

	// If the daemon socket already exists, remove it so we can take over listening.
	if _, err := xos.SocketStat(socketPath); err == nil {
		_ = os.Remove(socketPath)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		fatal(err)
	}
	d.closeOnExit(ln)

	// Detect when the socket is closed.
	go func() {
		d.exit <- detectSocketClose(ln, socketPath)
	}()
	return ln
}

func failedPreconditionError(msg, typ, desc string) error {
	st, err := status.New(codes.FailedPrecondition, msg).WithDetails(
		&errdetails.PreconditionFailure{
			Violations: []*errdetails.PreconditionFailure_Violation{
				{
					Type:        typ,
					Description: desc,
				},
			},
		},
	)
	if err != nil {
		panic(err)
	}
	return st.Err()
}

func ErrInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
	resp, err = handler(ctx, req)
	if errors.Is(err, conf.ErrInvalidRefreshToken) {
		return nil, failedPreconditionError("invalid refresh token", "INVALID_REFRESH_TOKEN", "invalid refresh token")
	} else if errors.Is(err, conf.ErrNotLoggedIn) {
		return nil, status.Error(codes.Unauthenticated, "not logged in")
	}
	return resp, err
}

func (d *Daemon) serveDaemon() {
	log.Info().Stringer("addr", d.Daemon.Addr()).Msg("serving daemon")
	srv := grpc.NewServer(grpc.UnaryInterceptor(ErrInterceptor))
	daemonpb.RegisterDaemonServer(srv, d.Server)
	d.exit <- srv.Serve(d.Daemon)
}

func (d *Daemon) serveRuntime() {
	log.Info().Stringer("addr", d.Runtime.Addr()).Msg("serving runtime")
	rec := trace2.NewRecorder(d.Trace)
	srv := engine.NewServer(d.RunMgr, rec)
	d.exit <- http.Serve(d.Runtime, srv)
}

func (d *Daemon) serveDBProxy() {
	log.Info().Stringer("addr", d.DBProxy.Addr()).Msg("serving dbproxy")
	d.exit <- d.ClusterMgr.ServeProxy(d.DBProxy)
}

func (d *Daemon) serveMCP() {
	log.Info().Stringer("addr", d.MCP.Addr()).Msg("serving mcp")
	d.exit <- d.MCPMgr.Serve(d.MCP)
}

func (d *Daemon) serveObjects() {
	log.Info().Stringer("addr", d.ObjectStorage.Addr()).Msg("serving object storage")
	d.exit <- d.PublicBuckets.Serve(d.ObjectStorage)
}

func (d *Daemon) serveDash() {
	log.Info().Stringer("addr", d.Dash.Addr()).Msg("serving dash")
	srv := dash.NewServer(d.Apps, d.RunMgr, d.NS, d.Trace, d.Dash.Port())
	d.exit <- http.Serve(d.Dash, srv)
}

func (d *Daemon) serveDebug() {
	log.Info().Stringer("addr", d.Debug.Addr()).Msg("serving debug")

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	d.exit <- http.Serve(d.Debug, mux)
}

// listenTCPRetry listens for TCP connections on the given port, retrying
// in the background if it's already in use.
func (d *Daemon) listenTCPRetry(component string, addrOverride option.Option[string], defaultPort uint16) *retryingTCPListener {
	addr, err := parseInterface(addrOverride.GetOrElse("127.0.0.1:0"))
	if err != nil {
		log.Fatal().Str("component", component).Err(err).Msg("failed to parse interface")
	}
	if addr.Port() == 0 {
		addr = netip.AddrPortFrom(addr.Addr(), defaultPort)
	}
	ln := listenLocalhostTCP(component, addr)
	d.closeOnExit(ln)
	return ln
}

func (d *Daemon) openDB() *sql.DB {
	dir, err := conf.Dir()
	if err != nil {
		fatal(err)
	} else if err := os.MkdirAll(dir, 0755); err != nil {
		fatal(err)
	}

	dbPath := filepath.Join(dir, "encore.db")

	// Create the database file if it doesn't exist, as
	// we've observed some failures to open the database file when it doesn't already exist.
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		if f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_WRONLY, 0600); err == nil {
			_ = f.Close()
		}
	}

	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?cache=shared&_journal=wal&_txlock=immediate", dbPath))
	if err != nil {
		fatal(err)
	}

	// Initialize db schema
	if err := d.runDBMigrations(db); err != nil {
		fatalf("unable to migrate management database: %v", err)
	}
	d.closeOnExit(db)

	return db
}

//go:embed migrations
var dbMigrations embed.FS

func (d *Daemon) runDBMigrations(db *sql.DB) error {
	{
		// Convert old-style schema definition to golang-migrate, if necessary.
		var isLegacy bool
		err := db.QueryRow(`
			SELECT COUNT(*) > 0 FROM pragma_table_info('schema_migrations') WHERE name = 'dummy'
		`).Scan(&isLegacy)
		if err != nil {
			return err
		} else if isLegacy {
			_, _ = db.Exec("DROP TABLE schema_migrations;")
		}
	}

	src, err := iofs.New(dbMigrations, "migrations")
	if err != nil {
		return fmt.Errorf("read db migrations: %v", err)
	}
	instance, err := sqlite3.WithInstance(db, &sqlite3.Config{})
	if err != nil {
		return fmt.Errorf("initialize migration instance: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "encore", instance)
	if err != nil {
		return fmt.Errorf("setup migrate instance: %v", err)
	}

	err = m.Up()
	if errors.Is(err, migrate.ErrNoChange) {
		return nil
	}

	// If we have a dirty migration, reset the dirty flag and try again.
	// This is safe since all migrations run inside transactions.
	var dirty migrate.ErrDirty
	if errors.As(err, &dirty) {
		// Find the version that preceded the dirty version so
		// we can force the migration to that version and then
		// re-apply the migration.
		var prevVer uint
		prevVer, err = src.Prev(uint(dirty.Version))
		targetVer := int(prevVer)
		if errors.Is(err, fs.ErrNotExist) {
			// No previous migration exists
			targetVer = database.NilVersion
		} else if err != nil {
			return errors.Wrap(err, "failed to find previous version")
		}

		if err = m.Force(targetVer); err == nil {
			err = m.Up()
		}
	}

	return err
}

// detectSocketClose polls for the unix socket at socketPath to be removed
// or changed to a different underlying inode.
func detectSocketClose(ln *net.UnixListener, socketPath string) error {
	orig, err := xos.SocketStat(socketPath)
	if err != nil {
		return err
	}

	// When this function exits, the socket has been changed.
	// In that case, don't unlink the socket since it has already been changed.
	defer ln.SetUnlinkOnClose(false)

	// Sleep until the socket changes
	errs := 0
	for {
		time.Sleep(200 * time.Millisecond)
		fi, err := xos.SocketStat(socketPath)
		if errors.Is(err, fs.ErrNotExist) {
			// Socket was removed; don't remove it again
			return nil
		} else if err != nil {
			errs++
			if errs == 3 {
				return err
			}
			time.Sleep(1 * time.Second)
			continue
		}
		if !xos.SameSocket(orig, fi) {
			return nil
		}
	}
}

func (d *Daemon) closeOnExit(c io.Closer) {
	d.close = append(d.close, c)
}

func (d *Daemon) closeAll() {
	for _, c := range d.close {
		_ = c.Close()
	}
}

type bailout struct {
	err error
}

func fatal(err error) {
	panic(bailout{err})
}

func fatalf(format string, args ...interface{}) {
	panic(bailout{fmt.Errorf(format, args...)})
}

func handleBailout(err *error) {
	if e := recover(); e != nil {
		if b, ok := e.(bailout); ok {
			*err = b.err
		} else {
			panic(e)
		}
	}
}

// redirectLogOutput redirects the global logger to also write to a file.
func redirectLogOutput() error {
	logPath := env.EncoreDaemonLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	log.Info().Msgf("writing output to %s", logPath)

	zerolog.TimeFieldFormat = time.RFC3339Nano
	consoleWriter := zerolog.ConsoleWriter{
		Out:           os.Stderr,
		FieldsExclude: []string{zerolog.ErrorStackFieldName},
	}
	consoleWriter.FormatExtra = eerror.ZeroLogConsoleExtraFormatter
	consoleWriter.TimeFormat = time.TimeOnly
	zerolog.ErrorStackMarshaler = eerror.ZeroLogStackMarshaller
	log.Logger = log.With().Caller().Stack().Logger().Output(io.MultiWriter(consoleWriter, f))
	return nil
}

// retryingTCPListener is a TCP listener that attempts multiple times
// to listen on a given port. It is designed to handle race conditions
// between multiple daemon processes handing off to each other
// and the port still being in use momentarily.
type retryingTCPListener struct {
	component string
	addr      netip.AddrPort
	ctx       context.Context
	cancel    func() // call to cancel ctx

	// doneListening is closed when the underlying listener is open,
	// or it gave up due to an error.
	doneListening chan struct{}
	underlying    net.Listener
	listenErr     error
}

func listenLocalhostTCP(component string, addr netip.AddrPort) *retryingTCPListener {
	ctx, cancel := context.WithCancel(context.Background())
	ln := &retryingTCPListener{
		component:     component,
		addr:          addr,
		ctx:           ctx,
		cancel:        cancel,
		doneListening: make(chan struct{}),
	}
	go ln.listen()
	return ln
}

func (ln *retryingTCPListener) Accept() (net.Conn, error) {
	select {
	case <-ln.ctx.Done():
		return nil, net.ErrClosed
	case <-ln.doneListening:
		if ln.listenErr != nil {
			return nil, ln.listenErr
		}
		return ln.underlying.Accept()
	}
}

func (ln *retryingTCPListener) Close() error {
	ln.cancel()
	select {
	case <-ln.doneListening:
		if ln.listenErr == nil {
			return ln.underlying.Close()
		}
	default:
	}
	return nil
}

func (ln *retryingTCPListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IP(ln.addr.Addr().AsSlice()), Port: int(ln.addr.Port())}
}

func (ln *retryingTCPListener) ClientAddr() string {
	// If our addr is 0.0.0.0 or the ipv6 equivalent, return 127.0.0.1 instead
	// so that clients can connect to us.
	if ln.addr.Addr().IsUnspecified() {
		if ln.addr.Addr().Is6() {
			return fmt.Sprintf("[::1]:%d", ln.addr.Port())
		}
		return fmt.Sprintf("127.0.0.1:%d", ln.addr.Port())
	}
	return ln.addr.String()
}

func (ln *retryingTCPListener) Port() int {
	return int(ln.addr.Port())
}

func (ln *retryingTCPListener) listen() {
	defer close(ln.doneListening)

	logger := log.With().Str("component", ln.component).Int("port", ln.Port()).Logger()
	addr := ln.addr.String()

	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 50 * time.Millisecond
	b.MaxInterval = 500 * time.Millisecond
	b.MaxElapsedTime = 5 * time.Second

	ln.listenErr = backoff.Retry(func() (err error) {
		if err := ln.ctx.Err(); err != nil {
			return backoff.Permanent(err)
		}
		ln.underlying, err = net.Listen("tcp", addr)
		if err != nil {
			logger.Error().Err(ln.listenErr).Msg("unable to listen, retrying")
		}
		return err
	}, b)

	if ln.listenErr != nil {
		logger.Error().Err(ln.listenErr).Msg("unable to listen, giving up")
	} else {
		logger.Info().Msg("listening on port")
	}
}

func parseInterface(s string) (netip.AddrPort, error) {
	addr, portStr, _, err := splitAddrPort(s)
	if err != nil {
		return netip.AddrPort{}, err
	}

	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return netip.AddrPort{}, err
	}

	// Is addr a valid ip? If so we're done.
	if ip, err := netip.ParseAddr(addr); err == nil {
		return netip.AddrPortFrom(ip, uint16(port)), nil
	}

	// Otherwise perform name resolution.
	ips, err := net.LookupIP(addr)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if len(ips) == 0 {
		return netip.AddrPort{}, fmt.Errorf("no IP addresses found for %s", addr)
	}

	// Prefer IPv4 addresses.
	for _, ip := range ips {
		if ip.To4() != nil {
			if addr, err := netip.ParseAddr(ip.String()); err == nil {
				return netip.AddrPortFrom(addr, uint16(port)), nil
			}
		}
	}

	if addr, err := netip.ParseAddr(ips[0].String()); err == nil {
		return netip.AddrPortFrom(addr, uint16(port)), nil
	}
	return netip.AddrPort{}, fmt.Errorf("unable to parse IP address %s", addr)
}

// splitAddrPort splits s into an IP address string and a port
// string. It splits strings shaped like "foo:bar" or "[foo]:bar",
// without further validating the substrings. v6 indicates whether the
// ip string should parse as an IPv6 address or an IPv4 address, in
// order for s to be a valid ip:port string.
func splitAddrPort(s string) (ip, port string, v6 bool, err error) {
	i := strings.LastIndexByte(s, ':')
	if i == -1 {
		return "", "", false, errors.New("not an ip:port")
	}

	ip, port = s[:i], s[i+1:]
	if len(ip) == 0 {
		return "", "", false, errors.New("no IP")
	}
	if len(port) == 0 {
		return "", "", false, errors.New("no port")
	}
	if ip[0] == '[' {
		if len(ip) < 2 || ip[len(ip)-1] != ']' {
			return "", "", false, errors.New("missing ]")
		}
		ip = ip[1 : len(ip)-1]
		v6 = true
	}

	return ip, port, v6, nil
}
