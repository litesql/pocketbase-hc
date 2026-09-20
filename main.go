package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/litesql/go-ha"
	sqlv1 "github.com/litesql/go-ha/api/sql/v1"
	"github.com/litesql/pocketbase-ha/remote"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/ghupdate"
	"github.com/pocketbase/pocketbase/plugins/jsvm"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"
	"github.com/pocketbase/pocketbase/tools/osutils"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	bootstrap             = make(chan struct{})
	interceptor           = new(ChangeSetInterceptor)
	twoPhaseCommitWorkers = make(map[string]string)
)

func init() {
	drv.Options = []ha.Option{
		ha.WithName(os.Getenv("PB_NAME")),
		ha.WithWaitFor(bootstrap),
		ha.WithChangeSetInterceptor(interceptor),
	}

	rowIdentify := os.Getenv("PB_ROW_IDENTIFY")
	if rowIdentify != "" {
		switch rowIdentify {
		case string(ha.PK):
			drv.Options = append(drv.Options, ha.WithRowIdentify(ha.PK))
		case string(ha.Rowid):
			drv.Options = append(drv.Options, ha.WithRowIdentify(ha.Rowid))
		case string(ha.Full):
			drv.Options = append(drv.Options, ha.WithRowIdentify(ha.Full))
		default:
			panic("invaid PB_ROW_IDENTIFY: " + rowIdentify)
		}
	}
	if leader := os.Getenv("PB_STATIC_LEADER"); leader != "" {
		drv.Options = append(drv.Options, ha.WithLeaderProvider(&ha.StaticLeader{
			Target: leader,
		}))
	}
	if grpcPort := os.Getenv("PB_GRPC_PORT"); grpcPort != "" {
		port, err := strconv.Atoi(grpcPort)
		if err != nil {
			panic("invalid PB_GRPC_PORT value: " + err.Error())
		}
		drv.Options = append(drv.Options, ha.WithGrpcPort(port))
	}
	if grpcToken := os.Getenv("PB_GRPC_TOKEN"); grpcToken != "" {
		drv.Options = append(drv.Options, ha.WithGrpcToken(grpcToken))
	}

	if peers := os.Getenv("PB_PEERS"); peers != "" {
		for peer := range strings.SplitSeq(peers, ",") {
			worker, key, _ := strings.Cut(peer, "=")
			twoPhaseCommitWorkers[worker] = key
		}
		recovery2pcPath := os.Getenv("PB_2PC_RECOVERY_PATH")
		if recovery2pcPath == "" {
			recovery2pcPath = "pb_data"
		}
		os.MkdirAll(recovery2pcPath, os.ModePerm)
		recovery2pcDB, err := sql.Open(dbDriver, fmt.Sprintf("file:%s", filepath.Join(recovery2pcPath, "2pc_recovery.db")))
		if err != nil {
			panic("failed to start 2pc recovery database: " + err.Error())
		}

		pub, err := ha.NewTwoPhaseCommitPublisher(twoPhaseCommitWorkers, 60*time.Second, recovery2pcDB)
		if err != nil {
			panic(err)
		}
		drv.Options = append(drv.Options, ha.WithReplicationPublisher(pub))

	} else {
		drv.Options = append(drv.Options, ha.WithReplicationPublisher(ha.NewNoopPublisher()))
	}

	drv.Options = append(drv.Options, ha.WithReplicationSubscriber(ha.NewNoopSubscriber()))

	sql.Register("pb_hc", &drv)

	dbx.BuilderFuncMap["pb_hc"] = dbx.BuilderFuncMap["sqlite"]
}

func main() {
	app := pocketbase.NewWithConfig(pocketbase.Config{
		DBConnect: func(dbPath string) (*dbx.DB, error) {
			return dbx.Open("pb_hc", dbPath)
		},
	})

	var hooksDir string
	app.RootCmd.PersistentFlags().StringVar(
		&hooksDir,
		"hooksDir",
		"",
		"the directory with the JS app hooks",
	)

	var hooksWatch bool
	app.RootCmd.PersistentFlags().BoolVar(
		&hooksWatch,
		"hooksWatch",
		true,
		"auto restart the app on pb_hooks file change; it has no effect on Windows",
	)

	var hooksPool int
	app.RootCmd.PersistentFlags().IntVar(
		&hooksPool,
		"hooksPool",
		15,
		"the total prewarm goja.Runtime instances for the JS app hooks execution",
	)

	var migrationsDir string
	app.RootCmd.PersistentFlags().StringVar(
		&migrationsDir,
		"migrationsDir",
		"",
		"the directory with the user defined migrations",
	)

	var automigrate bool
	app.RootCmd.PersistentFlags().BoolVar(
		&automigrate,
		"automigrate",
		true,
		"enable/disable auto migrations",
	)

	var publicDir string
	app.RootCmd.PersistentFlags().StringVar(
		&publicDir,
		"publicDir",
		defaultPublicDir(),
		"the directory to serve static files",
	)

	var indexFallback bool
	app.RootCmd.PersistentFlags().BoolVar(
		&indexFallback,
		"indexFallback",
		true,
		"fallback the request to index.html on missing static path, e.g. when pretty urls are used with SPA",
	)

	app.RootCmd.ParseFlags(os.Args[1:])

	// ---------------------------------------------------------------
	// Plugins and hooks:
	// ---------------------------------------------------------------

	// load jsvm (pb_hooks and pb_migrations)
	jsvm.MustRegister(app, jsvm.Config{
		MigrationsDir: migrationsDir,
		HooksDir:      hooksDir,
		HooksWatch:    hooksWatch,
		HooksPoolSize: hooksPool,
	})

	// migrate command (with js templates)
	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{
		TemplateLang: migratecmd.TemplateLangJS,
		Automigrate:  automigrate,
		Dir:          migrationsDir,
	})

	// GitHub selfupdate
	ghupdate.MustRegister(app, app.RootCmd, ghupdate.Config{
		Owner:             "litesql",
		Repo:              "pocketbase-hc",
		ArchiveExecutable: "pocketbase-hc",
	})

	// Remote access to database via gRPC
	remote.Register(app.RootCmd)

	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		if len(twoPhaseCommitWorkers) > 0 {
			timeout := 60 * time.Second
			if checkPeersTimeout := os.Getenv("PB_CHECK_PEERS_TIMEOUT"); checkPeersTimeout != "" {
				var err error
				timeout, err = time.ParseDuration(checkPeersTimeout)
				if err != nil {
					return fmt.Errorf("invalid PB_CHECK_PEERS_TIMEOUT: %w", err)
				}
			}
			chErr := make(chan error, len(twoPhaseCommitWorkers))
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			for remote, token := range twoPhaseCommitWorkers {
				go checkPeerConn(ctx, remote, token, chErr)
			}
			for range len(twoPhaseCommitWorkers) {
				err := <-chErr
				if err != nil {
					return err
				}
			}
		}
		close(bootstrap)

		var dataDSN string
		for _, dsn := range ha.ListDSN() {
			if strings.HasSuffix(dsn, "data.db") {
				dataDSN = dsn
				break
			}
		}

		connector, ok := ha.LookupConnector(dataDSN)
		if !ok {
			return fmt.Errorf("connector not found")
		}
		slog.Info("waiting for the leader")
		<-connector.LeaderProvider().Ready()

		if connector.LeaderProvider().IsLeader() {
			// force sync token definition
			_, err := app.ConcurrentDB().Update("_collections",
				dbx.Params{"updated": time.Now().Format("2006-01-02 15:04:05.000Z")},
				dbx.In("name", "_superusers", "users")).Execute()
			if err != nil {
				return fmt.Errorf("failed to sync configure: %w", err)
			}
		}

		superuserEmail := os.Getenv("PB_SUPERUSER_EMAIL")
		superuserPass := os.Getenv("PB_SUPERUSER_PASS")
		if superuserEmail != "" && superuserPass != "" {

			superusersCol, err := app.FindCachedCollectionByNameOrId(core.CollectionNameSuperusers)
			if err != nil {
				return fmt.Errorf("failed to fetch %q collection: %w", core.CollectionNameSuperusers, err)
			}

			superuser, err := app.FindAuthRecordByEmail(superusersCol, superuserEmail)
			if err != nil {
				superuser = core.NewRecord(superusersCol)
			}

			superuser.SetEmail(superuserEmail)
			superuser.SetPassword(superuserPass)

			if err := app.Save(superuser); err != nil {
				return fmt.Errorf("failed to set superuser account: %w", err)
			}
		}

		timeout := 10 * time.Second
		se.Router.BindFunc(apis.WrapStdMiddleware(connector.ForwardToLeader(timeout, "POST", "PUT", "PATCH", "DELETE")))
		return se.Next()
	})

	app.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		ha.Shutdown()
		return e.Next()
	})

	interceptor.app = app
	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

type ChangeSetInterceptor struct {
	app core.App
}

func (i *ChangeSetInterceptor) BeforeApply(cs *ha.ChangeSet, _ *sql.Conn) (skip bool, err error) {
	return false, nil
}

func (i *ChangeSetInterceptor) AfterApply(cs *ha.ChangeSet, _ *sql.Conn, err error) error {
	var reloadCollections, reloadSettings bool
	for _, change := range cs.Changes {
		if change.Table == "_collections" {
			reloadCollections = true
		}
		if change.Table == "_params" {
			reloadSettings = true
		}
		m := ModelFromChange(change, err)
		if m == nil {
			continue
		}
		m.TriggerAfterEvent(i.app)
	}
	if err == nil {
		if reloadCollections {
			i.app.ReloadCachedCollections()
		}
		if reloadSettings {
			i.app.ReloadSettings()
		}
	}
	return err
}

var _ core.Model = &Model{}

type Model struct {
	tableName string
	pk        any
	oldPk     any
	new       bool
	eventType string
	err       error
}

func ModelFromChange(c ha.Change, err error) *Model {
	var m Model
	switch c.Operation {
	case "INSERT":
		m.new = true
		m.eventType = core.ModelEventTypeCreate
	case "UPDATE":
		m.oldPk = c.PKOldValues()[0]
		m.eventType = core.ModelEventTypeUpdate
	case "DELETE":
		m.oldPk = c.PKOldValues()[0]
		m.eventType = core.ModelEventTypeDelete
	default:
		return nil
	}
	m.tableName = c.Table
	m.pk = c.PKNewValues()[0]
	m.err = err
	return &m
}

func (m *Model) TableName() string {
	return m.tableName
}

func (m *Model) PK() any {
	return m.pk
}

func (m *Model) LastSavedPK() any {
	return m.oldPk
}

func (m *Model) IsNew() bool {
	return m.new
}

func (m *Model) MarkAsNew() {
	m.oldPk = nil
	m.new = true
}

func (m *Model) MarkAsNotNew() {
	m.oldPk = m.pk
	m.new = false
}

func (m *Model) TriggerAfterEvent(app core.App) {
	event := new(core.ModelEvent)
	event.App = app
	event.Context = context.Background()
	event.Type = m.eventType
	event.Model = m
	switch m.eventType {
	case core.ModelEventTypeCreate:
		m.triggerAfterCreate(app, event)
	case core.ModelEventTypeUpdate:
		m.triggerAfterUpdate(app, event)
	case core.ModelEventTypeDelete:
		m.triggerAfterDelete(app, event)
	}
}

func (m *Model) triggerAfterCreate(app core.App, event *core.ModelEvent) {
	if m.err != nil {
		app.OnModelAfterCreateError().Trigger(&core.ModelErrorEvent{
			ModelEvent: *event,
			Error:      m.err,
		})
		return
	}
	app.OnModelAfterCreateSuccess().Trigger(event)
}

func (m *Model) triggerAfterUpdate(app core.App, event *core.ModelEvent) {
	if m.err != nil {
		app.OnModelAfterUpdateError().Trigger(&core.ModelErrorEvent{
			ModelEvent: *event,
			Error:      m.err,
		})
		return
	}
	app.OnModelAfterUpdateSuccess().Trigger(event)
}

func (m *Model) triggerAfterDelete(app core.App, event *core.ModelEvent) {
	if m.err != nil {
		app.OnModelAfterDeleteError().Trigger(&core.ModelErrorEvent{
			ModelEvent: *event,
			Error:      m.err,
		})
		return
	}
	app.OnModelAfterDeleteSuccess().Trigger(event)
}

// the default pb_public dir location is relative to the executable
func defaultPublicDir() string {
	if osutils.IsProbablyGoRun() {
		return "./pb_public"
	}

	return filepath.Join(os.Args[0], "../pb_public")
}

func checkPeerConn(ctx context.Context, remote, token string, chErr chan error) {
	u, err := url.Parse(remote)
	if err != nil {
		slog.Error("parse url", "error", err)
		chErr <- fmt.Errorf("invalid PB_PEERS: %w", err)
	}

	var dialOpts []grpc.DialOption

	if strings.HasPrefix(remote, "http://") {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})))
	}
	if token != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(grpcCredentials{token: token}))
	}

	cc, err := grpc.NewClient(u.Host, dialOpts...)
	if err != nil {
		slog.Error("grpc connect", "error", err)
		chErr <- err
		return
	}
	defer cc.Close()

	for {
		select {
		case <-ctx.Done():
			chErr <- ctx.Err()
			return
		case <-time.Tick(1 * time.Second):
			slog.Info("checking peer", "remote", remote)
			client, err := sqlv1.NewDatabaseServiceClient(cc).ChangeSet(ctx)
			if err != nil {
				continue
			}
			defer client.CloseSend()

			err = client.Send(&sqlv1.ChangeSetRequest{
				Type: sqlv1.CangeSetRequestType_CHANGESET_REQUEST_TYPE_PING,
			})
			if err != nil {
				chErr <- err
				return
			}

			resp, err := client.Recv()
			if err != nil {
				chErr <- err
				return
			}

			if resp.Error != "" {
				chErr <- fmt.Errorf("ping error: %w", err)
				return
			}

			chErr <- nil
		}
	}
}

type grpcCredentials struct {
	token string
}

func (c grpcCredentials) GetRequestMetadata(ctx context.Context, in ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": c.token,
	}, nil
}

func (c grpcCredentials) RequireTransportSecurity() bool {
	return false
}
