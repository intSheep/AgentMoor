package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"agentmoor/internal/adapter"
	"agentmoor/internal/asset"
	"agentmoor/internal/kubejobs"
	"agentmoor/internal/platform"
	"agentmoor/internal/task"
	"agentmoor/internal/worker"
)

func commandFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func platformCommand(ctx context.Context, args []string, out io.Writer, registry *adapter.Registry) error {
	f := commandFlags(args[0])
	dir := f.String("data", "", "private platform directory")
	switch args[0] {
	case "init":
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if *dir == "" || f.NArg() != 0 {
			return fmt.Errorf("init --data DIR")
		}
		if err := os.MkdirAll(*dir, 0700); err != nil {
			return err
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		key, err := os.CreateTemp(*dir, ".signing-*")
		if err != nil {
			return err
		}
		defer os.Remove(key.Name())
		_, err = key.Write(b)
		if err == nil {
			err = key.Sync()
		}
		if err = errors.Join(err, key.Close()); err != nil {
			return err
		}
		// Publish only a complete key; concurrent init must preserve the winner.
		if err = os.Link(key.Name(), filepath.Join(*dir, "signing.key")); err != nil {
			return err
		}
		parent, err := os.Open(*dir)
		if err != nil {
			return err
		}
		defer parent.Close()
		return parent.Sync()
	case "issue":
		authPolicy := f.String("auth-policy", "", "private authentication policy file")
		tenant := f.String("tenant", "", "tenant")
		project := f.String("project", "", "project")
		runtime := f.String("runtime", "", "logical runtime")
		agent := f.String("adapter", "", "adapter")
		write := f.Bool("write", false, "allow publish")
		gc := f.Bool("gc", false, "collect orphans")
		submit := f.Bool("submit", false, "allow task submission")
		execute := f.Bool("execute", false, "allow task execution and asset publication")
		ttl := f.Duration("ttl", time.Hour, "token lifetime")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if (*dir == "" && *authPolicy == "") || f.NArg() != 0 {
			return fmt.Errorf("issue requires --data or --auth-policy and a scope")
		}
		now := time.Now()
		grant := platform.Grant{Scope: asset.Scope{Tenant: *tenant, Project: *project, Runtime: *runtime, Adapter: *agent}, Read: true, Write: *write || *execute, Submit: *submit, Execute: *execute, Collect: *gc, Expires: now.Add(*ttl).Unix()}
		var token string
		var err error
		if *authPolicy != "" {
			token, err = platform.IssueAuthPolicy(*authPolicy, grant, now)
		} else {
			var key []byte
			key, err = os.ReadFile(filepath.Join(*dir, "signing.key"))
			if err == nil {
				token, err = platform.Issue(key, grant, now)
			}
		}
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, token)
		return err
	case "serve":
		authPolicy := f.String("auth-policy", "", "private authentication policy file, reloaded for each request")
		jobsFile := f.String("turn-jobs", "", "in-cluster follow-up Job profiles (JSON)")
		cert := f.String("tls-cert", "", "TLS certificate PEM")
		keyFile := f.String("tls-key", "", "TLS private key PEM")
		addr := f.String("listen", "127.0.0.1:7401", "listen address")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if *dir == "" || f.NArg() != 0 {
			return fmt.Errorf("serve --data DIR [--listen ADDRESS]")
		}
		var key []byte
		var err error
		if *authPolicy != "" {
			if _, err = platform.LoadAuthPolicy(*authPolicy); err != nil {
				return err
			}
		} else {
			key, err = os.ReadFile(filepath.Join(*dir, "signing.key"))
			if err != nil {
				return err
			}
			if len(key) != 32 {
				return fmt.Errorf("invalid signing key")
			}
		}
		var objects platform.Objects = platform.DiskObjects{Dir: filepath.Join(*dir, "objects")}
		if ep := os.Getenv("AGENTMOOR_S3_ENDPOINT"); ep != "" {
			objects, err = platform.NewS3(ep, os.Getenv("AGENTMOOR_S3_BUCKET"), os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), os.Getenv("AWS_SESSION_TOKEN"), os.Getenv("AGENTMOOR_S3_HTTP") != "1")
			if err != nil {
				return err
			}
		}
		catalog, err := platform.OpenCatalog(ctx, *dir, os.Getenv("AGENTMOOR_POSTGRES_DSN"), objects)
		if err != nil {
			return err
		}
		defer catalog.Close()
		incoming := filepath.Join(*dir, "incoming")
		if err := platform.PrepareUploads(incoming); err != nil {
			return err
		}
		if disk, ok := objects.(platform.DiskObjects); ok {
			if err := platform.PrepareUploads(disk.Dir); err != nil {
				return err
			}
		}
		enabled := map[string]bool{}
		files := map[string]bool{}
		for _, a := range registry.All() {
			_, enabled[a.Descriptor().ID] = a.(task.Executor)
			_, files[a.Descriptor().ID] = a.(task.FileExecutor)
		}
		if (*cert == "") != (*keyFile == "") {
			return errors.New("TLS requires certificate and key")
		}
		api := platform.API{Catalog: catalog, Secret: key, AuthPolicy: *authPolicy, Temp: incoming, TaskAdapters: enabled, FileTaskAdapters: files}
		if *jobsFile != "" {
			dispatch, err := kubejobs.InCluster(*jobsFile)
			if err != nil {
				return err
			}
			api.TurnEnabled = func(scope asset.Scope) bool {
				a, err := registry.Lookup(scope.Adapter)
				if err != nil {
					return false
				}
				_, ok := a.(task.Continuation)
				return ok && dispatch.Enabled(scope)
			}
			dispatchCtx, stopDispatch := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { defer close(done); dispatch.Run(dispatchCtx, catalog) }()
			defer func() { stopDispatch(); <-done }()
		}
		server := &http.Server{Addr: *addr, Handler: api.Handler(), BaseContext: func(net.Listener) context.Context { return ctx }, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 64 << 10}
		ln, err := net.Listen("tcp", *addr)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "listening", ln.Addr())
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			<-ctx.Done()
			closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := server.Shutdown(closeCtx); err != nil {
				_ = server.Close()
			}
		}()
		if *cert != "" {
			err = server.ServeTLS(ln, *cert, *keyFile)
		} else {
			err = server.Serve(ln)
		}
		if errors.Is(err, http.ErrServerClosed) {
			<-stopped
			return nil
		}
		return err
	}
	return errors.New(usage)
}

func workerCommand(ctx context.Context, args []string, out io.Writer, registry *adapter.Registry) error {
	f := commandFlags(args[0])
	cfg := f.String("config", "", "trusted worker configuration")
	var kind, id, dest, turn string
	var version int64
	var interval time.Duration
	var periodic bool
	switch args[0] {
	case "restore", "restore-native":
		f.StringVar(&kind, "kind", "", "asset type")
		f.StringVar(&id, "id", "", "external asset ID")
		f.StringVar(&dest, "to", "", "new destination")
		f.Int64Var(&version, "version", 0, "zero selects latest")
	case "restore-sessions":
		f.StringVar(&dest, "to", "", "new history directory")
	case "run-task":
		f.StringVar(&turn, "turn", "", "follow-up turn ID")
		f.DurationVar(&interval, "wait", 2*time.Minute, "wait for a SaaS submission to this allocation")
	case "watch":
		f.DurationVar(&interval, "interval", time.Minute, "reconciliation interval")
		f.BoolVar(&periodic, "periodic-only", false, "use timer only")
	case "resolve":
		f.Int64Var(&version, "expected-version", 0, "reviewed platform version")
	}
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if *cfg == "" || f.NArg() != 0 {
		return fmt.Errorf("%s requires --config FILE", args[0])
	}
	c, err := worker.LoadConfig(*cfg)
	if err != nil {
		return err
	}
	a, err := registry.Lookup(c.Scope.Adapter)
	if err != nil {
		return err
	}
	var client *platform.Client
	if file := os.Getenv("AGENTMOOR_TOKEN_FILE"); file != "" {
		client, err = platform.NewFileClient(c.Endpoint, file)
	} else {
		client, err = platform.NewClient(c.Endpoint, os.Getenv("AGENTMOOR_TOKEN"))
	}
	if err != nil {
		return err
	}
	e := worker.Engine{Config: c, Adapter: a, Client: client}
	switch args[0] {
	case "run-task":
		if turn != "" {
			return e.RunTurn(ctx, turn)
		}
		return e.RunTask(ctx, interval)
	case "sync":
		return e.Sync(ctx)
	case "watch":
		return e.Watch(ctx, interval, !periodic, os.Stderr)
	case "resolve":
		return e.Rebase(ctx, version)
	case "restore-sessions":
		if dest == "" {
			return fmt.Errorf("restore-sessions requires --to")
		}
		return e.RestoreSessions(ctx, dest)
	case "restore-native":
		if id == "" || dest == "" || version < 0 {
			return fmt.Errorf("restore-native requires --id and --to")
		}
		return e.RestoreNative(ctx, id, version, dest)
	case "restore":
		k := asset.Key{Kind: kind, ID: id}
		if !k.Valid() || dest == "" || version < 0 {
			return fmt.Errorf("restore requires --kind --id --to and a nonnegative version")
		}
		return e.Restore(ctx, k, version, dest)
	case "status":
		local, err := worker.LocalStatus(c)
		if err != nil {
			return err
		}
		var remote json.RawMessage
		g, platformErr := client.Context(ctx)
		if platformErr == nil && g.Scope != c.Scope {
			platformErr = fmt.Errorf("credential scope mismatch")
		}
		if platformErr == nil {
			platformErr = client.JSON(ctx, "GET", "/v1/status", nil, &remote)
		}
		result := map[string]any{"worker": local, "platform": remote}
		if platformErr != nil {
			result["platform_error"] = platformErr.Error()
		}
		return errors.Join(json.NewEncoder(out).Encode(result), platformErr)
	case "assets", "gc":
		g, err := client.Context(ctx)
		if err != nil {
			return err
		}
		if g.Scope != c.Scope {
			return fmt.Errorf("credential scope mismatch")
		}
		method := "GET"
		if args[0] == "gc" {
			method = "POST"
		}
		var value json.RawMessage
		if err = client.JSON(ctx, method, "/v1/"+args[0], nil, &value); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(value))
		return err
	}
	return fmt.Errorf("unknown command %s", strconv.Quote(args[0]))
}
