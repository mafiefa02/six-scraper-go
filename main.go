package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/sync/singleflight"
)

var (
	sixBaseURL = envOrDefault("SIX_BASE_URL", "https://six.itb.ac.id")
	listenAddr = envOrDefault("LISTEN_ADDR", ":8080")
	cacheTTL   = mustParseDuration(envOrDefault("CACHE_TTL", "5m"))
)

var (
	studentIDRe      = regexp.MustCompile(`mahasiswa:(\d+)`)
	semesterRe       = regexp.MustCompile(`\+(\d{4}-\d)`)
	whitespaceRe     = regexp.MustCompile(`\s+`)
	studentIDValidRe = regexp.MustCompile(`^\d+$`)
	semesterValidRe  = regexp.MustCompile(`^\d{4}-\d$`)
)

type ScheduleEntry struct {
	ISODay    int      `json:"iso_day"`
	Timezone  string   `json:"timezone"`
	Dates     []string `json:"dates"`
	StartTime string   `json:"start_time"`
	EndTime   string   `json:"end_time"`
	Rooms     []string `json:"rooms"`
	Activity  string   `json:"activity"`
	Method    string   `json:"method"`
}

type CourseClass struct {
	Code      string          `json:"code"`
	Name      string          `json:"name"`
	SKS       int             `json:"sks"`
	ClassNo   string          `json:"class_no"`
	Quota     int             `json:"quota"`
	Lecturers []string        `json:"lecturers"`
	Notes     *string         `json:"notes"`
	Schedules []ScheduleEntry `json:"schedules"`
}

type UserResponse struct {
	StudentID string `json:"student_id"`
	Semester  string `json:"semester"`
}

type APIResponse struct {
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Meta    *Meta  `json:"meta,omitempty"`
	Error   string `json:"error,omitempty"`
}

type Meta struct {
	FetchedAt time.Time `json:"fetched_at"`
	Cached    bool      `json:"cached"`
}

type cacheEntry struct {
	data      []CourseClass
	fetchedAt time.Time
	expiresAt time.Time
}

var (
	scheduleCache = make(map[string]cacheEntry)
	cacheMu       sync.RWMutex
	sfGroup       singleflight.Group
)

var errUnauthenticated = errors.New("khongguan token is expired or invalid")

// errStatus maps known sentinel errors to their HTTP status code.
func errStatus(err error) int {
	if errors.Is(err, errUnauthenticated) {
		return http.StatusUnauthorized
	}
	return http.StatusBadGateway
}

var httpClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	},
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustParseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		log.Fatalf("invalid CACHE_TTL %q: %v", s, err)
	}
	return d
}

func main() {
	mux := http.NewServeMux()
	mux.Handle("/api/user", logRequest(http.HandlerFunc(userHandler)))
	mux.Handle("/api/schedule", logRequest(http.HandlerFunc(scheduleHandler)))

	srv := &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startCacheEviction(ctx)

	go func() {
		log.Printf("Server starting on %s...", listenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}

// Wraps a handler and logs method, path, status, and total duration.
func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s status=%d duration=%s", r.Method, r.URL.String(), sw.status, time.Since(start))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if !sw.wroteHeader {
		sw.status = code
		sw.wroteHeader = true
	}
	sw.ResponseWriter.WriteHeader(code)
}

// Creates an outbound request to SIX, forwarding the caller's khongguan token as a cookie.
func newSIXRequest(ctx context.Context, targetURL string, r *http.Request) (*http.Request, error) {
	v := r.Header.Get("X-Six-Khongguan")
	if v == "" {
		return nil, fmt.Errorf("missing required khongguan token")
	}

	req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
	if err != nil {
		return nil, err
	}

	req.AddCookie(&http.Cookie{Name: "khongguan", Value: v})
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	return req, nil
}

// Performs a GET against targetURL (forwarding auth from r) and returns the parsed document.
func fetchDoc(targetURL string, r *http.Request) (*goquery.Document, *http.Response, error) {
	req, err := newSIXRequest(r.Context(), targetURL, r)
	if err != nil {
		return nil, nil, err
	}

	fetchStart := time.Now()
	resp, err := httpClient.Do(req)
	fetchDuration := time.Since(fetchStart)
	if err != nil {
		log.Printf("fetch error url=%s duration=%s err=%v", targetURL, fetchDuration, err)
		return nil, nil, err
	}

	log.Printf("fetch url=%s status=%d duration=%s", targetURL, resp.StatusCode, fetchDuration)

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, resp, fmt.Errorf("upstream returned %s", resp.Status)
	}

	parseStart := time.Now()
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, resp, err
	}
	log.Printf("parse url=%s duration=%s", targetURL, time.Since(parseStart))
	if doc.Find("a.loginlink").Length() > 0 {
		return nil, resp, errUnauthenticated
	}
	return doc, resp, nil
}

func writeSuccess(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(APIResponse{Success: true, Data: data}); err != nil {
		log.Printf("json encode error: %v", err)
	}
}

func writeSuccessWithMeta(w http.ResponseWriter, data any, meta *Meta) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(APIResponse{Success: true, Data: data, Meta: meta}); err != nil {
		log.Printf("json encode error: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(APIResponse{Success: false, Error: msg}); err != nil {
		log.Printf("json encode error: %v", err)
	}
}

func userHandler(w http.ResponseWriter, r *http.Request) {
	doc, _, err := fetchDoc(sixBaseURL+"/home", r)
	if err != nil {
		writeError(w, errStatus(err), err.Error())
		return
	}

	var studentID string
	doc.Find("a[href*='mahasiswa:']").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		href, _ := s.Attr("href")
		if m := studentIDRe.FindStringSubmatch(href); len(m) > 1 {
			studentID = m[1]
			return false
		}
		return true
	})

	if studentID == "" {
		writeError(w, http.StatusNotFound, "Could not find student ID on /home")
		return
	}

	redirectURL := fmt.Sprintf("%s/app/mahasiswa:%s/kelas", sixBaseURL, studentID)
	req, err := newSIXRequest(r.Context(), redirectURL, r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		writeError(w, http.StatusBadGateway, "unexpected upstream status: "+resp.Status)
		return
	}

	// A redirect to /home means the token has expired.
	if resp.Request.URL.Path == "/home" {
		writeError(w, http.StatusUnauthorized, errUnauthenticated.Error())
		return
	}

	finalURL := resp.Request.URL.String()
	m := semesterRe.FindStringSubmatch(finalURL)
	if len(m) < 2 {
		writeError(w, http.StatusNotFound, "Could not infer semester from redirect URL: "+finalURL)
		return
	}

	writeSuccess(w, UserResponse{StudentID: studentID, Semester: m[1]})
}

func scheduleHandler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	studentID := query.Get("student_id")
	semester := query.Get("semester")

	if studentID == "" || semester == "" {
		writeError(w, http.StatusBadRequest, "Missing student_id or semester query parameters")
		return
	}

	if !studentIDValidRe.MatchString(studentID) {
		writeError(w, http.StatusBadRequest, "invalid student_id format")
		return
	}
	if !semesterValidRe.MatchString(semester) {
		writeError(w, http.StatusBadRequest, "invalid semester format")
		return
	}

	targetURL := buildScheduleURL(studentID, semester, query)
	refresh := query.Get("refresh") == "true"

	if !refresh {
		if entry, ok := getCached(targetURL); ok {
			log.Printf("cache hit student_id=%s semester=%s", studentID, semester)
			writeSuccessWithMeta(w, entry.data, &Meta{FetchedAt: entry.fetchedAt, Cached: true})
			return
		}
	}
	log.Printf("cache miss student_id=%s semester=%s refresh=%v", studentID, semester, refresh)

	// Validate auth per-caller before joining the singleflight group, so a caller
	// with no token cannot piggyback on another caller's in-flight fetch.
	if r.Header.Get("X-Six-Khongguan") == "" {
		writeError(w, http.StatusBadGateway, "missing required khongguan token")
		return
	}

	type sfResult struct {
		classes   []CourseClass
		fetchedAt time.Time
	}

	v, err, _ := sfGroup.Do(targetURL, func() (any, error) {
		// Use a background-derived context so that a disconnect by the first caller
		// does not abort the fetch for all other waiters on this key.
		fetchCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		doc, _, err := fetchDoc(targetURL, r.WithContext(fetchCtx))
		if err != nil {
			return nil, err
		}
		now := time.Now()
		classes := parseClasses(doc)
		log.Printf("parsed classes=%d student_id=%s semester=%s", len(classes), studentID, semester)
		setCache(targetURL, classes, now)
		return sfResult{classes: classes, fetchedAt: now}, nil
	})

	if err != nil {
		writeError(w, errStatus(err), err.Error())
		return
	}

	result := v.(sfResult)
	writeSuccessWithMeta(w, result.classes, &Meta{FetchedAt: result.fetchedAt, Cached: false})
}

func getCached(key string) (cacheEntry, bool) {
	cacheMu.RLock()
	defer cacheMu.RUnlock()
	entry, ok := scheduleCache[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return cacheEntry{}, false
	}
	return entry, true
}

func setCache(key string, data []CourseClass, fetchedAt time.Time) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	scheduleCache[key] = cacheEntry{data: data, fetchedAt: fetchedAt, expiresAt: time.Now().Add(cacheTTL)}
}

func evictExpired() {
	now := time.Now()
	cacheMu.Lock()
	defer cacheMu.Unlock()
	for k, v := range scheduleCache {
		if now.After(v.expiresAt) {
			delete(scheduleCache, k)
		}
	}
}

func startCacheEviction(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(cacheTTL)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				evictExpired()
			case <-ctx.Done():
				return
			}
		}
	}()
}

func buildScheduleURL(studentID, semester string, query url.Values) string {
	u := fmt.Sprintf("%s/app/mahasiswa:%s+%s/kelas/jadwal/kuliah", sixBaseURL, studentID, semester)

	q := url.Values{}
	for _, key := range []string{"fakultas", "prodi", "pekan", "kegiatan"} {
		if v := query.Get(key); v != "" {
			q.Set(key, v)
		}
	}
	if encoded := q.Encode(); encoded != "" {
		u += "?" + encoded
	}
	return u
}

func parseClasses(doc *goquery.Document) []CourseClass {
	var classes []CourseClass

	doc.Find("table.table tbody tr").Each(func(_ int, s *goquery.Selection) {
		cells := s.Find("td, th")
		if cells.Length() < 10 {
			return
		}

		sks, _ := strconv.Atoi(strings.TrimSpace(cells.Eq(4).Text()))
		quota, _ := strconv.Atoi(strings.TrimSpace(cells.Eq(6).Text()))

		var notes *string
		if n := collapseWhitespace(cells.Eq(8).Text()); n != "" {
			notes = &n
		}

		class := CourseClass{
			Code:      strings.TrimSpace(cells.Eq(2).Text()),
			Name:      strings.TrimSpace(cells.Eq(3).Text()),
			SKS:       sks,
			ClassNo:   strings.TrimSpace(cells.Eq(5).Text()),
			Quota:     quota,
			Lecturers: parseLecturers(cells.Eq(7)),
			Notes:     notes,
			Schedules: parseSchedules(cells.Eq(9)),
		}

		if class.Code != "" {
			classes = append(classes, class)
		}
	})

	return classes
}

func parseLecturers(cell *goquery.Selection) []string {
	var lecturers []string
	cell.Find("ul li").Each(func(_ int, li *goquery.Selection) {
		if name := strings.TrimSpace(li.Text()); name != "" {
			lecturers = append(lecturers, name)
		}
	})
	return lecturers
}

type scheduleKey struct {
	ISODay                        int
	StartTime, EndTime, Activity, Method string
}

func parseSchedules(cell *goquery.Selection) []ScheduleEntry {
	type accumulator struct {
		dates     []string
		dateSeen  map[string]bool
		rooms     []string
		roomSeen  map[string]bool
	}

	var order []scheduleKey
	acc := make(map[scheduleKey]*accumulator)

	cell.Find("li").Each(func(_ int, li *goquery.Selection) {
		text := collapseWhitespace(li.Text())
		if text == "" || strings.Contains(text, "Tampilkan semua") {
			return
		}

		parts := strings.Split(text, "/")
		if len(parts) < 6 {
			return
		}

		start, end := parseScheduleTime(strings.TrimSpace(parts[2]))
		key := scheduleKey{
			ISODay:    idDays[strings.TrimSpace(parts[0])],
			StartTime: start,
			EndTime:   end,
			Activity:  strings.TrimSpace(parts[4]),
			Method:    strings.TrimSpace(parts[5]),
		}
		date := parseIndonesianDate(strings.TrimSpace(parts[1]))
		room := strings.TrimSpace(parts[3])

		a, ok := acc[key]
		if !ok {
			a = &accumulator{dateSeen: make(map[string]bool), rooms: []string{}, roomSeen: make(map[string]bool)}
			acc[key] = a
			order = append(order, key)
		}
		if !a.dateSeen[date] {
			a.dates = append(a.dates, date)
			a.dateSeen[date] = true
		}
		if room != "-" && !a.roomSeen[room] {
			a.rooms = append(a.rooms, room)
			a.roomSeen[room] = true
		}
	})

	schedules := make([]ScheduleEntry, 0, len(order))
	for _, key := range order {
		a := acc[key]
		schedules = append(schedules, ScheduleEntry{
			ISODay:    key.ISODay,
			Timezone:  "Asia/Jakarta",
			Dates:     a.dates,
			StartTime: key.StartTime,
			EndTime:   key.EndTime,
			Rooms:     a.rooms,
			Activity:  key.Activity,
			Method:    key.Method,
		})
	}
	return schedules
}

// Trims and collapses all runs of whitespace into a single space.
func collapseWhitespace(s string) string {
	return strings.TrimSpace(whitespaceRe.ReplaceAllString(s, " "))
}

var idDays = map[string]int{
	"Senin":  1,
	"Selasa": 2,
	"Rabu":   3,
	"Kamis":  4,
	"Jumat":  5,
	"Sabtu":  6,
	"Minggu": 7,
}

var idMonths = map[string]string{
	"Jan": "01", "Feb": "02", "Mar": "03", "Apr": "04",
	"Mei": "05", "Jun": "06", "Jul": "07", "Agu": "08",
	"Sep": "09", "Okt": "10", "Nov": "11", "Des": "12",
}

// Splits a time range like "13.00 - 15.50" into "13:00" and "15:50".
func parseScheduleTime(s string) (start, end string) {
	parts := strings.SplitN(s, " - ", 2)
	if len(parts) != 2 {
		return strings.ReplaceAll(s, ".", ":"), ""
	}
	return strings.ReplaceAll(strings.TrimSpace(parts[0]), ".", ":"),
		strings.ReplaceAll(strings.TrimSpace(parts[1]), ".", ":")
}

// Parses an Indonesian date string like "11 Feb 2026" into ISO 8601 "2026-02-11".
// Returns the original string if parsing fails.
func parseIndonesianDate(s string) string {
	parts := strings.Fields(s)
	if len(parts) != 3 {
		return s
	}
	month, ok := idMonths[parts[1]]
	if !ok {
		return s
	}
	day := parts[0]
	if len(day) == 1 {
		day = "0" + day
	}
	return parts[2] + "-" + month + "-" + day
}
