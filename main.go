package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	_ "github.com/lib/pq"
)

var logger *slog.Logger

type Review struct {
	isbn10 string
	isbn13 string
	did    string
	text   string
	rating int16
	path   string
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

	dbUser := os.Getenv("POSTGRES_USER")
	dbName := os.Getenv("POSTGRES_DATABASE")
	dbPassword := os.Getenv("POSTGRES_PASSWORD")

	if dbUser == "" || dbName == "" || dbPassword == "" {
		logger.Error("required environment variables not set",
			"POSTGRES_USER", dbUser == "",
			"POSTGRES_DATABASE", dbName == "",
			"POSTGRES_PASSWORD", dbPassword == "")
		return
	}

	connStr := fmt.Sprintf("user=%s dbname=%s password=%s sslmode=disable",
		dbUser, dbName, dbPassword)

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		logger.Error("error connecting to database", "err", err)
		return
	}

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
				if !strings.HasPrefix(op.Path, "app.bsky.feed.post") {
					continue
				}

				switch op.Action {
				case "create":
					handleCreatePost(ctx, evt, op, db)
					break
				case "delete":
					handleDeletePost(ctx, evt, op, db)
					break
				}
			}
			return nil
		},
	}

	sched := sequential.NewScheduler("myscheduler", rscb.EventHandler)
	events.HandleRepoStream(context.Background(), con, sched, logger)
}

func handleDeletePost(ctx context.Context, evt *atproto.SyncSubscribeRepos_Commit, op *atproto.SyncSubscribeRepos_RepoOp, db *sql.DB) {
	did := evt.Repo
	path := op.Path

	const reviewDeleteQuery = "DELETE FROM reviews WHERE path = $1 and did = $2"

	res, err := db.Exec(reviewDeleteQuery, path, did)
	if err != nil {
		logger.Error("error deleting review", "err", err)
	}

	if cnt, err := res.RowsAffected(); err == nil && cnt > 0 {
		logger.Info("review deleted", "did", did, "path", path)
	}
}

func handleCreatePost(ctx context.Context, evt *atproto.SyncSubscribeRepos_Commit, op *atproto.SyncSubscribeRepos_RepoOp, db *sql.DB) {
	pst, err := parsePost(ctx, evt, op)
	if err != nil {
		logger.Error("error parsing post", "err", err)
		return
	}

	if !strings.Contains(pst.Text, hashtag) {
		return
	}

	review, err := extractReviewdata(pst.Text)
	if err != nil {
		logger.Error("error extracting review data", "err", err)
		return
	}
	review.did = evt.Repo
	review.path = op.Path

	logger.Info("review data", "did", review.did, "isbn10", review.isbn10, "isbn13", review.isbn13, "rating", review.rating, "text", review.text)

	if err := insert(db, review); err != nil {
		logger.Error("error inserting review", "err", err)
	}
}

func insert(db *sql.DB, review *Review) error {
	const bookQuery = `SELECT true from books WHERE isbn10 = $1 and isbn13 = $2`
	var exists bool
	err := db.QueryRow(bookQuery, review.isbn10, review.isbn13).Scan(&exists)
	if err != nil && err == sql.ErrNoRows {
		// If book doesn't exist, try to fetch data from GoodReads
		err = hydrateBook(review, db)
		if err != nil {
			logger.Error("error hydrating book", "err", err)
			// Fallback to basic book insert if GoodReads data fetch fails
			const basicBookInsertQuery = `
				INSERT INTO books (isbn10, isbn13, created_at, updated_at)
				VALUES ($1, $2, NOW(), NOW())
				ON CONFLICT (isbn10, isbn13) DO NOTHING`

			if review.isbn10 != "" || review.isbn13 != "" {
				_, err := db.Exec(basicBookInsertQuery, review.isbn10, review.isbn13)
				if err != nil {
					return fmt.Errorf("error inserting book: %v", err)
				}
			}
		}
	}

	// Then insert the review
	const reviewQuery = `
		INSERT INTO reviews (isbn10, isbn13, did, text, rating, path, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
		ON CONFLICT ON CONSTRAINT reviews_pkey DO UPDATE SET
			did = EXCLUDED.did,
			text = EXCLUDED.text,
			rating = EXCLUDED.rating,
			updated_at = NOW()
	`
	_, err = db.Exec(reviewQuery, review.isbn10, review.isbn13, review.did, review.text, review.rating, review.path)
	if err != nil {
		return fmt.Errorf("error inserting review: %v", err)
	}
	return nil
}

func hydrateBook(review *Review, db *sql.DB) error {
	url := fmt.Sprintf("https://www.goodreads.com/search?utf8=%%E2%%9C%%93&query=%s", review.isbn13)
	resp, err := http.Get(url)
	if err != nil {
		logger.Error("failed to fetch GoodReads data", "err", err)
		return err
	} else {
		defer resp.Body.Close()

		if resp.StatusCode == 200 {
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return err

			}
			re := regexp.MustCompile(`<script\s+type="application/ld\+json">(.*?)</script>`)
			matches := re.FindSubmatch(body)

			if len(matches) > 1 {
				var jsonData map[string]interface{}
				err = json.Unmarshal(matches[1], &jsonData)
				if err != nil {
					return err
				}
				// Enhanced book insert with GoodReads data
				const bookInsertQuery = `
					INSERT INTO books (isbn10, isbn13, title, author, cover_image_url, created_at, updated_at)
					VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
					ON CONFLICT (isbn10, isbn13) DO NOTHING`

				title, _ := jsonData["name"].(string)
				authors, _ := jsonData["author"].([]interface{})
				var author string
				if len(authors) > 0 {
					if authorMap, ok := authors[0].(map[string]interface{}); ok {
						author, _ = authorMap["name"].(string)
					}
				}
				coverURL, _ := jsonData["image"].(string)

				_, err = db.Exec(bookInsertQuery, review.isbn10, review.isbn13, title, author, coverURL)
				if err != nil {
					logger.Error("error inserting book with metadata", "err", err)
					return err
				}
				return nil
			} else {
				return errors.New("no GoodReads data found")
			}
		}
	}
	return nil
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

	// Case insensitive ISBN regex with flexible whitespace around colon
	isbnRegex := regexp.MustCompile(`(?i)isbn\s*:\s*([0-9-]{10,17})`)
	matches := isbnRegex.FindStringSubmatch(str)

	if len(matches) > 1 {
		// Remove all non-digit characters (including dashes and spaces)
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
