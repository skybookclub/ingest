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
	"regexp"
	"strconv"
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

type Review struct {
	isbn10 string
	isbn13 string
	did    string
	text   string
	rating int16
}

const (
	hashtag = "#skybookclub"
)

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
					continue
				}

				if !strings.Contains(pst.Text, hashtag) {
					continue
				}

				review, err := extractReviewdata(pst.Text)
				if err != nil {
					logger.Error("error extracting review data", "err", err)
					continue
				}
				review.did = evt.Repo

				logger.Info("review data", "did", review.did, "isbn10", review.isbn10, "isbn13", review.isbn13, "rating", review.rating, "text", review.text)

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

func convertISBN10to13(isbn10 string) (string, error) {
	// Validate ISBN-10 length
	if len(isbn10) != 10 {
		return "", fmt.Errorf("invalid ISBN-10 length: %d", len(isbn10))
	}

	// Take first 9 digits and prepend 978
	isbn13 := "978" + isbn10[:9]

	// Calculate check digit
	sum := 0
	for i := 0; i < 12; i++ {
		digit := int(isbn13[i] - '0')
		if i%2 == 0 {
			sum += digit
		} else {
			sum += digit * 3
		}
	}

	checkDigit := (10 - (sum % 10)) % 10
	isbn13 = isbn13 + strconv.Itoa(checkDigit)

	return isbn13, nil
}

func convertISBN13to10(isbn13 string) (string, error) {
	// Validate ISBN-13 length and prefix
	if len(isbn13) != 13 {
		return "", fmt.Errorf("invalid ISBN-13 length: %d", len(isbn13))
	}
	if !strings.HasPrefix(isbn13, "978") {
		return "", fmt.Errorf("ISBN-13 must start with 978 to convert to ISBN-10")
	}

	// Remove "978" prefix
	isbn10 := isbn13[3:12]

	// Calculate check digit
	sum := 0
	for i := 0; i < 9; i++ {
		digit := int(isbn10[i] - '0')
		sum += digit * (10 - i)
	}
	checksum := (11 - (sum % 11)) % 11

	// Convert check digit to string (X for 10)
	var checkDigit string
	if checksum == 10 {
		checkDigit = "X"
	} else {
		checkDigit = strconv.Itoa(checksum)
	}

	return isbn10 + checkDigit, nil
}

func extractReviewdata(str string) (*Review, error) {
	review := &Review{}

	// ISBN regex pattern matching 10 or 13 digits, allowing hyphens
	isbnRegex := regexp.MustCompile(`isbn:([0-9-]{10,17})`)
	matches := isbnRegex.FindStringSubmatch(str)

	if len(matches) > 1 {
		isbn := regexp.MustCompile(`[^0-9]`).ReplaceAllString(matches[1], "")
		if len(isbn) == 10 {
			review.isbn10 = isbn
			if isbn13, err := convertISBN10to13(isbn); err == nil {
				review.isbn13 = isbn13
			}
		} else if len(isbn) == 13 {
			review.isbn13 = isbn
			if isbn10, err := convertISBN13to10(isbn); err == nil {
				review.isbn10 = isbn10
			}
		}
	}

	ratingRegex := regexp.MustCompile(`([0-5])/5`)
	matches = ratingRegex.FindStringSubmatch(str)
	if len(matches) > 1 {
		rating, err := strconv.ParseInt(matches[1], 10, 16)
		if err != nil {
			return nil, fmt.Errorf("error parsing rating: %v", err)
		}

		review.rating = int16(rating)
	}

	// Clean the review text by removing patterns
	cleanText := str
	cleanText = isbnRegex.ReplaceAllString(cleanText, "")                   // Remove ISBN
	cleanText = ratingRegex.ReplaceAllString(cleanText, "")                 // Remove rating
	cleanText = regexp.MustCompile(hashtag).ReplaceAllString(cleanText, "") // Remove hashtags
	cleanText = strings.TrimSpace(cleanText)                                // Remove extra whitespace

	review.text = cleanText
	return review, nil
}
