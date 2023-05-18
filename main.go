package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/betterstack-community/wikipedia-demo/logger"
	"github.com/rs/xid"
	"go.uber.org/zap"
)

var tpl *template.Template

var HTTPClient = http.Client{
	Timeout: 30 * time.Second,
}

type WikipediaSearchResponse struct {
	BatchComplete string `json:"batchcomplete"`
	Continue      struct {
		Continue string `json:"continue"`
		Sroffset int    `json:"sroffset"`
	} `json:"continue"`
	Query struct {
		Search []struct {
			Ns        int       `json:"ns"`
			Title     string    `json:"title"`
			PageID    int       `json:"pageid"`
			Size      int       `json:"size"`
			WordCount int       `json:"wordcount"`
			Snippet   string    `json:"snippet"`
			Timestamp time.Time `json:"timestamp"`
		} `json:"search"`
		SearchInfo struct {
			TotalHits int `json:"totalhits"`
		} `json:"searchinfo"`
	} `json:"query"`
}

type Search struct {
	Query      string
	TotalPages int
	NextPage   int
	Results    *WikipediaSearchResponse
}

func (s *Search) IsLastPage() bool {
	return s.NextPage >= s.TotalPages
}

func (s *Search) CurrentPage() int {
	if s.NextPage == 1 {
		return s.NextPage
	}

	return s.NextPage - 1
}

func (s *Search) PreviousPage() int {
	return s.CurrentPage() - 1
}

type handlerWithError func(w http.ResponseWriter, r *http.Request) error

func (fn handlerWithError) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l := logger.FromCtx(r.Context())

	err := fn(w, r)
	if err != nil {
		l.Error("server error", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func newLoggingResponseWriter(w http.ResponseWriter) *loggingResponseWriter {
	return &loggingResponseWriter{w, http.StatusOK}
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

func indexHandler(w http.ResponseWriter, r *http.Request) error {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return nil
	}

	buf := &bytes.Buffer{}

	err := tpl.Execute(buf, nil)
	if err != nil {
		return err
	}

	_, err = buf.WriteTo(w)

	return err
}

func searchWikipedia(
	searchQuery string,
	pageSize, resultsOffset int,
) (*WikipediaSearchResponse, error) {
	endpoint := fmt.Sprintf(
		"https://en.wikipedia.org/w/api.php?action=query&list=search&prop=info&inprop=url&utf8=&format=json&origin=*&srlimit=%d&srsearch=%s&sroffset=%d",
		pageSize,
		searchQuery,
		resultsOffset,
	)

	fmt.Println(endpoint)

	resp, err := HTTPClient.Get(endpoint)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respData, _ := httputil.DumpResponse(resp, true)

		return nil, fmt.Errorf(
			"non 200 OK response from Wikipedia API: %s",
			string(respData),
		)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var searchResponse WikipediaSearchResponse

	err = json.Unmarshal(body, &searchResponse)
	if err != nil {
		return nil, err
	}

	return &searchResponse, nil
}

func searchHandler(w http.ResponseWriter, r *http.Request) error {
	u, err := url.Parse(r.URL.String())
	if err != nil {
		return err
	}

	params := u.Query()
	searchQuery := params.Get("q")

	pageNum := params.Get("page")
	if pageNum == "" {
		pageNum = "1"
	}

	l := logger.FromCtx(r.Context())

	l = l.With(
		zap.String("search_query", searchQuery),
		zap.String("page_num", pageNum),
	)

	l.Info(
		fmt.Sprintf(
			"incoming search query '%s' on page '%s'",
			searchQuery,
			pageNum,
		),
	)

	nextPage, err := strconv.Atoi(pageNum)
	if err != nil {
		return err
	}

	pageSize := 20

	resultsOffset := (nextPage - 1) * pageSize

	searchResponse, err := searchWikipedia(searchQuery, pageSize, resultsOffset)
	if err != nil {
		return err
	}

	l.Debug(
		"search response from Wikipedia",
		zap.Any("wikipedia_search_response", searchResponse),
	)

	totalHits := searchResponse.Query.SearchInfo.TotalHits

	search := &Search{
		Query:      searchQuery,
		Results:    searchResponse,
		TotalPages: int(math.Ceil(float64(totalHits) / float64(pageSize))),
		NextPage:   nextPage + 1,
	}

	buf := &bytes.Buffer{}

	err = tpl.Execute(buf, search)
	if err != nil {
		return err
	}

	_, err = buf.WriteTo(w)
	if err != nil {
		return err
	}

	l.Info("search query succeeded without errors")

	return nil
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		l := logger.Get()

		correlationID := xid.New().String()

		ctx := context.WithValue(r.Context(), "correlation_id", correlationID)

		r = r.WithContext(ctx)

		l = l.With(zap.String("correlation_id", correlationID))

		w.Header().Add("X-Correlation-ID", correlationID)

		lrw := newLoggingResponseWriter(w)

		r = r.WithContext(logger.WithContext(ctx, l))

		defer func() {
			panicVal := recover()
			if panicVal != nil {
				lrw.statusCode = http.StatusInternalServerError // ensure that the status code is updated
				panic(panicVal)
			}

			l.Info(
				"incoming request",
				zap.String("method", r.Method),
				zap.String("url", r.URL.RequestURI()),
				zap.String("user_agent", r.UserAgent()),
				zap.Int("status_code", lrw.statusCode),
				zap.Duration("elapsed_ms", time.Since(start)),
			)
		}()

		next.ServeHTTP(lrw, r)
	})
}

func htmlSafe(str string) template.HTML {
	return template.HTML(str)
}

var err error

func init() {
	l := logger.Get()

	tpl, err = template.New("index.html").Funcs(template.FuncMap{
		"htmlSafe": htmlSafe,
	}).ParseFiles("index.html")
	if err != nil {
		l.Fatal("Unable to initialize HTML templates", zap.Error(err))
	}
}

func main() {
	l := logger.Get()

	fs := http.FileServer(http.Dir("assets"))

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	mux := http.NewServeMux()
	mux.Handle("/assets/", http.StripPrefix("/assets/", fs))
	mux.Handle("/search", handlerWithError(searchHandler))
	mux.Handle("/", handlerWithError(indexHandler))

	l.Info(
		"Starting Wikipedia App Server on port: "+port,
		zap.String("port", port),
	)

	l.Fatal(
		"Wikipedia App Server Closed",
		zap.Error(http.ListenAndServe(":"+port, requestLogger(mux))),
	)
}
