// Command localsend-cli is a headless LocalSend client: it runs as a daemon
// that accepts incoming transfers, and as a one-shot sender.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zebroc/localsend-cli/internal/discovery"
	"github.com/zebroc/localsend-cli/internal/identity"
	"github.com/zebroc/localsend-cli/internal/peers"
	"github.com/zebroc/localsend-cli/internal/proto"
	"github.com/zebroc/localsend-cli/internal/send"
	"github.com/zebroc/localsend-cli/internal/server"
	"github.com/zebroc/localsend-cli/internal/ui"
)

// version is overridable at build time:
//
//	go build -ldflags "-X main.version=1.2.3"
var version = "dev"

type options struct {
	daemon   bool
	list     bool
	showVer  bool
	dstDir   string
	port     int
	alias    string
	pin      string
	timeout  time.Duration
	parallel int
	noTLS    bool
	group    string
	announce time.Duration
	verbose  bool
}

func main() {
	opts := parseFlags()

	if opts.showVer {
		fmt.Printf("localsend-cli %s (protocol %s)\n", version, proto.Version)
		return
	}

	ui.Verbose = opts.verbose

	var err error
	switch {
	case opts.daemon:
		err = runDaemon(opts)
	case opts.list:
		err = runList(opts)
	default:
		err = runSend(opts, flag.Args())
	}

	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Interrupted by the user; nothing to report.
			os.Exit(130)
		}
		ui.Errorf("%v", err)
		os.Exit(1)
	}
}

func parseFlags() options {
	var opts options
	flag.BoolVar(&opts.daemon, "daemon", false, "run as a daemon that accepts all incoming transfers")
	flag.BoolVar(&opts.list, "list", false, "list the LocalSend devices on the local network")
	flag.BoolVar(&opts.showVer, "version", false, "print the version and exit")
	flag.StringVar(&opts.dstDir, "dstdir", "~/Downloads", "directory for received files (daemon mode)")
	flag.IntVar(&opts.port, "port", proto.DefaultPort, "TCP/UDP port to use")
	flag.StringVar(&opts.alias, "alias", "", "device name shown to other devices (default: hostname)")
	flag.StringVar(&opts.pin, "pin", "", "PIN to require (daemon) or to send (sender)")
	flag.DurationVar(&opts.timeout, "timeout", 5*time.Second, "how long to wait for devices to answer")
	flag.IntVar(&opts.parallel, "parallel", 1, "number of files to upload at the same time")
	flag.BoolVar(&opts.noTLS, "no-tls", false, "serve plain HTTP instead of HTTPS")
	flag.StringVar(&opts.group, "group", proto.DefaultMulticastGroup, "multicast group for discovery")
	flag.DurationVar(&opts.announce, "announce-interval", 5*time.Minute,
		"how often the daemon re-announces itself (0 = only at startup)")
	flag.BoolVar(&opts.verbose, "v", false, "verbose logging")

	flag.Usage = usage
	flag.Parse()
	return opts
}

func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprintf(out, `localsend-cli — LocalSend for the command line

Usage:
  localsend-cli --daemon [--dstdir DIR]      receive: accept everything, store it in DIR
  localsend-cli <target> <path>...           send files and/or directories
  localsend-cli --list                       show the devices found on this network

<target> is the alias of a device ("phone"), or an address ("192.168.1.5",
"192.168.1.5:%d", "phone.local:%d"). Aliases are matched case-insensitively,
exact matches first. <path> may be a file or a directory; directories are sent
recursively and arrive as directories.

Examples:
  localsend-cli --daemon --dstdir ~/Downloads
  localsend-cli phone report.pdf photo.jpg
  localsend-cli phone ~/Downloads/
  localsend-cli --pin 123456 192.168.1.5 backup.zip

Flags:
`, proto.DefaultPort, proto.DefaultPort)
	flag.PrintDefaults()
}

func runDaemon(opts options) error {
	// Output usually ends up in a log (e.g. multilog), so stamp every line.
	ui.Timestamps = true

	id, err := identity.Load(opts.alias)
	if err != nil {
		return err
	}

	dstDir, err := resolveDir(opts.dstDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return fmt.Errorf("destination directory: %w", err)
	}

	cache := peers.Load()

	srv, err := server.New(server.Config{
		Identity: id,
		Port:     opts.port,
		UseTLS:   !opts.noTLS,
		DstDir:   dstDir,
		PIN:      opts.pin,
		OnPeer:   cache.Put,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ui.Infof("localsend-cli %s: %q on port %d (%s), saving to %s",
		version, id.Alias, srv.Port(), srv.Protocol(), dstDir)
	ui.Debugf("fingerprint %s", id.Fingerprint)
	if opts.pin != "" {
		ui.Infof("a PIN is required for incoming transfers")
	}

	discoveryCfg := discovery.Config{
		Identity:      id,
		Port:          srv.Port(),
		Protocol:      srv.Protocol(),
		Group:         opts.group,
		MulticastPort: opts.port,
	}

	// Listening for announcements is a nice-to-have: we announce ourselves
	// either way and peers can always reach us by address, so a failure here
	// must not stop the daemon.
	if listener, err := discovery.Listen(discoveryCfg, opts.port, true, cache.Put); err != nil {
		ui.Warnf("cannot listen for announcements: %v", err)
		ui.Warnf("another LocalSend instance is holding UDP port %d. This daemon still announces "+
			"itself and stays reachable, but it will not answer other devices' announcements. "+
			"Quit the other instance or give both a separate --port.", opts.port)
	} else {
		defer listener.Close()
		go listener.Run(ctx)
	}

	go announceLoop(ctx, discoveryCfg, opts.announce)
	go saveCacheLoop(ctx, cache)

	err = srv.Serve(ctx)
	if saveErr := cache.Save(); saveErr != nil {
		ui.Debugf("saving peer cache: %v", saveErr)
	}
	if err != nil {
		return err
	}
	ui.Infof("stopped")
	return nil
}

func runSend(opts options, args []string) error {
	if len(args) == 0 {
		flag.Usage()
		return errors.New("missing target device and files")
	}
	if len(args) == 1 {
		return fmt.Errorf("no files given — usage: localsend-cli %s <path>...", args[0])
	}

	target, paths := args[0], args[1:]

	entries, err := send.Collect(paths)
	if err != nil {
		return err
	}

	id, err := identity.Load(opts.alias)
	if err != nil {
		return err
	}
	cache := peers.Load()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	device, err := discovery.Resolve(ctx, target, discovery.ResolveConfig{
		ScanConfig: discovery.ScanConfig{
			Identity:      id,
			Group:         opts.group,
			MulticastPort: opts.port,
			Timeout:       opts.timeout,
			OnDevice:      cache.Put,
		},
		Cached: cache.Lookup(target),
	})
	if saveErr := cache.Save(); saveErr != nil {
		ui.Debugf("saving peer cache: %v", saveErr)
	}
	if err != nil {
		return err
	}

	return send.Send(ctx, send.Options{
		Identity: id,
		Target:   device,
		Entries:  entries,
		PIN:      opts.pin,
		Parallel: opts.parallel,
		Port:     opts.port,
	})
}

func runList(opts options) error {
	id, err := identity.Load(opts.alias)
	if err != nil {
		return err
	}
	cache := peers.Load()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ui.Infof("scanning for %s…", opts.timeout)
	devices, err := discovery.Scan(ctx, discovery.ScanConfig{
		Identity:      id,
		Group:         opts.group,
		MulticastPort: opts.port,
		Timeout:       opts.timeout,
		OnDevice:      cache.Put,
	})
	if saveErr := cache.Save(); saveErr != nil {
		ui.Debugf("saving peer cache: %v", saveErr)
	}
	if err != nil {
		return err
	}

	if len(devices) == 0 {
		ui.Infof("no devices found")
		return nil
	}

	rows := make([][]string, 0, len(devices))
	for _, device := range devices {
		rows = append(rows, []string{
			device.Alias,
			fmt.Sprintf("%s:%d", device.IP, device.Port),
			device.Protocol,
			device.DeviceType,
			device.DeviceModel,
			shorten(device.Fingerprint),
		})
	}
	ui.Table([]string{"ALIAS", "ADDRESS", "PROTO", "TYPE", "MODEL", "FINGERPRINT"}, rows)
	return nil
}

// announceLoop re-announces periodically so we stay visible after network
// changes or a sleep/wake cycle.
func announceLoop(ctx context.Context, cfg discovery.Config, interval time.Duration) {
	discovery.Announce(ctx, cfg)
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			discovery.Announce(ctx, cfg)
		}
	}
}

func saveCacheLoop(ctx context.Context, cache *peers.Cache) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := cache.Save(); err != nil {
				ui.Debugf("saving peer cache: %v", err)
			}
		}
	}
}

// resolveDir expands a leading "~" and makes the path absolute. Defaults are
// not expanded by a shell, so we do it ourselves.
func resolveDir(dir string) (string, error) {
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(dir, "~"), "/"))
	}
	return filepath.Abs(dir)
}

func shorten(fingerprint string) string {
	if len(fingerprint) > 16 {
		return fingerprint[:16] + "…"
	}
	return fingerprint
}
