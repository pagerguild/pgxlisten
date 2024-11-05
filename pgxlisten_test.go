package pgxlisten_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxtest"
	"github.com/stretchr/testify/require"

	"github.com/jackc/pgxlisten"
)

var (
	connString            = os.Getenv("DATABASE_URL")
	defaultConnTestRunner pgxtest.ConnTestRunner
)

func init() {
	// Set up the default connection string from an environment variable or default
	if connString == "" {
		connString = "postgres://postgres:password@localhost/guilde?sslmode=disable"
	}

	// Customize the ConnTestRunner
	defaultConnTestRunner = pgxtest.ConnTestRunner{
		// CreateConfig generates a *pgx.ConnConfig from the connection string
		CreateConfig: func(ctx context.Context, t testing.TB) *pgx.ConnConfig {
			config, err := pgx.ParseConfig(connString)
			if err != nil {
				t.Fatalf("ParseConfig failed: %v", err)
			}
			return config
		},
		// Optional: setup actions right after connection creation, if needed
		AfterConnect: func(ctx context.Context, t testing.TB, conn *pgx.Conn) {
			// e.g., setting application-specific parameters or initializing extensions
		},
		// Optional: validation or cleanup actions after each test
		AfterTest: func(ctx context.Context, t testing.TB, conn *pgx.Conn) {
			// e.g., verifying connection state or cleaning up test artifacts
		},
		// CloseConn simply closes the connection, handling errors if they arise
		CloseConn: func(ctx context.Context, t testing.TB, conn *pgx.Conn) {
			if err := conn.Close(ctx); err != nil {
				t.Errorf("CloseConn failed: %v", err)
			}
		},
	}
}
func TestListenerListenDispatchesNotifications(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()

	defaultConnTestRunner.RunTest(ctx, t, func(ctx context.Context, t testing.TB, conn *pgx.Conn) {
		listener := &pgxlisten.Listener{
			Connect: func(ctx context.Context) (*pgx.Conn, error) {
				config := defaultConnTestRunner.CreateConfig(ctx, t)
				return pgx.ConnectConfig(ctx, config)
			},
		}

		fooChan := make(chan *pgconn.Notification)
		barChan := make(chan *pgconn.Notification)

		listener.Handle("foo", pgxlisten.HandlerFunc(func(ctx context.Context, notification *pgconn.Notification, conn *pgx.Conn) error {
			select {
			case fooChan <- notification:
			case <-ctx.Done():
			}
			return nil
		}))

		listener.Handle("bar", pgxlisten.HandlerFunc(func(ctx context.Context, notification *pgconn.Notification, conn *pgx.Conn) error {
			select {
			case barChan <- notification:
			case <-ctx.Done():
			}
			return nil
		}))

		listenerCtx, listenerCtxCancel := context.WithCancel(ctx)
		defer listenerCtxCancel()
		listenerDoneChan := make(chan struct{})

		go func() {
			listener.Listen(listenerCtx)
			close(listenerDoneChan)
		}()

		// No way to know when Listener is ready so wait a little.
		time.Sleep(2 * time.Second)

		type notificationTest struct {
			goChan  chan *pgconn.Notification
			channel string
			payload string
		}

		notificationTests := []notificationTest{
			{goChan: fooChan, channel: "foo", payload: "a"},
			{goChan: fooChan, channel: "foo", payload: "b"},
			{goChan: barChan, channel: "bar", payload: "c"},
			{goChan: fooChan, channel: "foo", payload: "d"},
			{goChan: barChan, channel: "bar", payload: "e"},
		}

		// Send all notifications.
		for i, nt := range notificationTests {
			_, err := conn.Exec(ctx, `select pg_notify($1, $2)`, nt.channel, nt.payload)
			require.NoErrorf(t, err, "%d", i)
		}

		// Receive all notifications.
		for i, nt := range notificationTests {
			select {
			case notification := <-nt.goChan:
				require.Equalf(t, nt.channel, notification.Channel, "%d", i)
				require.Equalf(t, nt.payload, notification.Payload, "%d", i)
			case <-ctx.Done():
				t.Fatalf("%d. %v", i, ctx.Err())
			}
		}

		listenerCtxCancel()

		// Wait for Listen to finish.
		select {
		case <-listenerDoneChan:
		case <-ctx.Done():
			t.Fatalf("ctx cancelled while waiting for Listen() to return: %v", ctx.Err())
		}
	})
}

type msgHandler struct {
	ctx context.Context
	ch  chan string
}

func (h *msgHandler) HandleNotification(ctx context.Context, notification *pgconn.Notification, conn *pgx.Conn) error {
	select {
	case h.ch <- notification.Payload:
	case <-ctx.Done():
	}
	return nil
}

func (h *msgHandler) HandleBacklog(ctx context.Context, channel string, conn *pgx.Conn) error {
	var msg string
	rows, err := conn.Query(ctx, `SELECT msg FROM pgxlisten_test`)
	if err != nil {
		return err
	}
	defer rows.Close()

	_, err = pgx.ForEachRow(rows, []any{&msg}, func() error {
		h.ch <- msg
		return nil
	})
	return err
}

func TestListenerListenHandlesBacklog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()

	ctr := defaultConnTestRunner
	ctr.AfterConnect = func(ctx context.Context, t testing.TB, conn *pgx.Conn) {
		_, err := conn.Exec(ctx, `drop table if exists pgxlisten_test;
create table pgxlisten_test (id int primary key generated by default as identity, msg text not null);
`)
		require.NoError(t, err)
	}
	ctr.AfterTest = func(ctx context.Context, t testing.TB, conn *pgx.Conn) {
		_, err := conn.Exec(ctx, `drop table if exists pgxlist_test;`)
		require.NoError(t, err)
	}

	ctr.RunTest(ctx, t, func(ctx context.Context, t testing.TB, conn *pgx.Conn) {
		backlogMsgs := []string{"a", "b", "c"}
		for _, msg := range backlogMsgs {
			_, err := conn.Exec(ctx, `insert into pgxlisten_test (msg) values ($1);`, msg)
			require.NoError(t, err)
		}

		listener := &pgxlisten.Listener{
			Connect: func(ctx context.Context) (*pgx.Conn, error) {
				config := ctr.CreateConfig(ctx, t)
				return pgx.ConnectConfig(ctx, config)
			},
		}

		fooChan := make(chan string, 8)

		handler := &msgHandler{
			ctx: ctx,
			ch:  fooChan,
		}

		listener.Handle("foo", handler)

		listenerCtx, listenerCtxCancel := context.WithCancel(ctx)
		defer listenerCtxCancel()
		listenerDoneChan := make(chan struct{})

		go func() {
			listener.Listen(listenerCtx)
			close(listenerDoneChan)
		}()

		// No way to know when Listener is ready so wait a little.
		time.Sleep(2 * time.Second)

		type notificationTest struct {
			payload string
		}

		notificationMsgs := []string{"d", "e"}

		// Send all notifications.
		for i, msg := range notificationMsgs {
			_, err := conn.Exec(ctx, `select pg_notify($1, $2)`, "foo", msg)
			require.NoErrorf(t, err, "%d", i)
		}

		// Receive all backlog notifications.
		for i, expected := range backlogMsgs {
			select {
			case actual := <-fooChan:
				require.Equalf(t, expected, actual, "%d", i)
			case <-ctx.Done():
				t.Fatalf("%d. %v", i, ctx.Err())
			}
		}

		// Receive all notifications.
		for i, expected := range notificationMsgs {
			select {
			case actual := <-fooChan:
				require.Equalf(t, expected, actual, "%d", i)
			case <-ctx.Done():
				t.Fatalf("%d. %v", i, ctx.Err())
			}
		}

		listenerCtxCancel()

		// Wait for Listen to finish.
		select {
		case <-listenerDoneChan:
		case <-ctx.Done():
			t.Fatalf("ctx cancelled while waiting for Listen() to return: %v", ctx.Err())
		}
	})
}
