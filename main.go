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

	logger.Info("dialing", "url", arg)
	d := websocket.DefaultDialer
	con, _, err := d.Dial(arg, http.Header{})
	if err != nil {
		logger.Error("error dialing", "err", err)
		return
	}

	logger.Info("Stream Started")
	defer func() {
		logger.Info("Stream Exited")
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

				pst, err := parsePost(ctx, evt, op)
				if err != nil {
					logger.Error("error parsing post", "err", err)
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

func parsePost(ctx context.Context, evt *atproto.SyncSubscribeRepos_Commit, op *atproto.SyncSubscribeRepos_RepoOp) (*appbsky.FeedPost, error) {
	rr, err := repo.ReadRepoFromCar(ctx, bytes.NewReader(evt.Blocks))
	if err != nil {
		return nil, fmt.Errorf("error reading repo from car: %v", err)
	}

	rc, rec, err := rr.GetRecord(ctx, op.Path)
	if err != nil {
		return nil, fmt.Errorf("error getting record: %v", err)
	}

	if lexutil.LexLink(rc) != *op.Cid {
		return nil, fmt.Errorf("mismatch in record and op cid: %s != %s", rc, *op.Cid)
	}

	banana := lexutil.LexiconTypeDecoder{
		Val: rec,
	}

	b, err := banana.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("error marshalling record: %v\n", err)
	}

	var pst = appbsky.FeedPost{}
	err = json.Unmarshal(b, &pst)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling record: %v\n", err)
	}
	return &pst, nil
}
