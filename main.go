package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bluesky-social/indigo/api/atproto"
	appbsky "github.com/bluesky-social/indigo/api/bsky"
	"github.com/bluesky-social/indigo/events"
	"github.com/bluesky-social/indigo/events/schedulers/sequential"
	lexutil "github.com/bluesky-social/indigo/lex/util"
	"github.com/bluesky-social/indigo/repo"
	"github.com/gorilla/websocket"
)

var logger *slog.Logger

func main() {
	// Initialize logger
	logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT)
	defer stop()

	logger.Info("starting application")

	arg := "wss://bsky.network/xrpc/com.atproto.sync.subscribeRepos"

	fmt.Println("dialing: ", arg)
	d := websocket.DefaultDialer
	con, _, err := d.Dial(arg, http.Header{})
	if err != nil {
		fmt.Printf("dial failure: %v", err)
		return
	}

	fmt.Println("Stream Started", time.Now().Format(time.RFC3339))
	defer func() {
		fmt.Println("Stream Exited", time.Now().Format(time.RFC3339))
	}()

	go func() {
		<-ctx.Done()
		_ = con.Close()
	}()

	rscb := &events.RepoStreamCallbacks{
		RepoCommit: func(evt *atproto.SyncSubscribeRepos_Commit) error {
			for _, op := range evt.Ops {
				if op.Action != "create" || !strings.HasPrefix(op.Path, "app.bsky.feed.post") {
					continue
				}

				rr, err := repo.ReadRepoFromCar(ctx, bytes.NewReader(evt.Blocks))
				if err != nil {
					fmt.Fprintf(os.Stderr, "error reading repo from car: %v\n", err)
					return nil
				}

				rc, rec, err := rr.GetRecord(ctx, op.Path)
				if err != nil {
					fmt.Fprintf(os.Stderr, "error getting record: %v\n", err)
					return nil
				}

				if lexutil.LexLink(rc) != *op.Cid {
					fmt.Fprintf(os.Stderr, "mismatch in record and op cid: %s != %s\n", rc, *op.Cid)
					return fmt.Errorf("mismatch in record and op cid: %s != %s", rc, *op.Cid)
				}

				banana := lexutil.LexiconTypeDecoder{
					Val: rec,
				}

				b, err := banana.MarshalJSON()
				if err != nil {
					fmt.Fprintf(os.Stderr, "error marshalling record: %v\n", err)
					return nil
				}

				var pst = appbsky.FeedPost{}
				err = json.Unmarshal(b, &pst)
				if err != nil {
					fmt.Fprintf(os.Stderr, "error unmarshalling record: %v\n", err)
					return nil
				}

				if strings.Contains(pst.Text, "#skybookclub") {
					fmt.Printf("post: %s\n", pst.Text)
				}
			}
			return nil
		},
	}

	sched := sequential.NewScheduler("myscheduler", rscb.EventHandler)
	events.HandleRepoStream(context.Background(), con, sched, logger)
}
