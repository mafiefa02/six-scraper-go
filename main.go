package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const sixBaseURL = "https://six.itb.ac.id"

var (
	studentIDRe  = regexp.MustCompile(`mahasiswa:(\d+)`)
	semesterRe   = regexp.MustCompile(`\+(\d{4}-\d)`)
	whitespaceRe = regexp.MustCompile(`\s+`)
)

type ScheduleEntry struct {
	Day      string `json:"day"`
	Time     string `json:"time"`
	Room     string `json:"room"`
	Activity string `json:"activity"`
	Method   string `json:"method"`
}

type CourseClass struct {
	Code      string          `json:"code"`
	Name      string          `json:"name"`
	SKS       int             `json:"sks"`
	ClassNo   string          `json:"class_no"`
	Quota     int             `json:"quota"`
	Lecturers []string        `json:"lecturers"`
	Notes     string          `json:"notes"`
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

var requiredCookies = []string{"nissin", "khongguan"}

const cacheTTL = 5 * time.Minute

type cacheEntry struct {
	data      []CourseClass
	fetchedAt time.Time
	expiresAt time.Time
}

var (
	scheduleCache = make(map[string]cacheEntry)
	cacheMu       sync.RWMutex
)

func main() {
	http.Handle("/api/user", logRequest(http.HandlerFunc(userHandler)))
	http.Handle("/api/schedule", logRequest(http.HandlerFunc(scheduleHandler)))

	fmt.Println("Server starting on :8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
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

// Creates an outbound request to SIX, forwarding auth cookies from the incoming request.
func newSIXRequest(targetURL string, r *http.Request) (*http.Request, error) {
	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return nil, err
	}

	for _, name := range requiredCookies {
		c, err := r.Cookie(name)
		if err != nil {
			return nil, fmt.Errorf("missing required cookie: %s", name)
		}
		req.AddCookie(c)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	return req, nil
}

// Performs a GET against targetURL (forwarding cookies from r) and returns the parsed document.
func fetchDoc(client *http.Client, targetURL string, r *http.Request) (*goquery.Document, *http.Response, error) {
	req, err := newSIXRequest(targetURL, r)
	if err != nil {
		return nil, nil, err
	}

	fetchStart := time.Now()
	resp, err := client.Do(req)
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

func newHTTPClient() *http.Client {
	return &http.Client{}
}

func userHandler(w http.ResponseWriter, r *http.Request) {
	client := newHTTPClient()

	// Get Student ID from /home
	doc, _, err := fetchDoc(client, sixBaseURL+"/home", r)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
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

	// Get Semester from redirect URL
	redirectURL := fmt.Sprintf("%s/app/mahasiswa:%s/kelas", sixBaseURL, studentID)
	req, err := newSIXRequest(redirectURL, r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Body.Close()

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

	client := newHTTPClient()
	doc, _, err := fetchDoc(client, targetURL, r)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	now := time.Now()
	classes := parseClasses(doc)
	log.Printf("parsed classes=%d student_id=%s semester=%s", len(classes), studentID, semester)
	setCache(targetURL, classes, now)
	writeSuccessWithMeta(w, classes, &Meta{FetchedAt: now, Cached: false})
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

		class := CourseClass{
			Code:      strings.TrimSpace(cells.Eq(2).Text()),
			Name:      strings.TrimSpace(cells.Eq(3).Text()),
			SKS:       sks,
			ClassNo:   strings.TrimSpace(cells.Eq(5).Text()),
			Quota:     quota,
			Lecturers: parseLecturers(cells.Eq(7)),
			Notes:     collapseWhitespace(cells.Eq(8).Text()),
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

func parseSchedules(cell *goquery.Selection) []ScheduleEntry {
	var schedules []ScheduleEntry
	seen := make(map[string]bool)

	cell.Find("li").Each(func(_ int, li *goquery.Selection) {
		text := collapseWhitespace(li.Text())
		if text == "" || strings.Contains(text, "Tampilkan semua") {
			return
		}

		parts := strings.Split(text, "/")
		if len(parts) < 6 {
			return
		}

		entry := ScheduleEntry{
			Day:      strings.TrimSpace(parts[0]),
			Time:     strings.TrimSpace(parts[2]),
			Room:     strings.TrimSpace(parts[3]),
			Activity: strings.TrimSpace(parts[4]),
			Method:   strings.TrimSpace(parts[5]),
		}

		key := entry.Day + "|" + entry.Time + "|" + entry.Room + "|" + entry.Activity + "|" + entry.Method
		if !seen[key] {
			schedules = append(schedules, entry)
			seen[key] = true
		}
	})

	return schedules
}

// Trims and collapses all runs of whitespace into a single space.
func collapseWhitespace(s string) string {
	return strings.TrimSpace(whitespaceRe.ReplaceAllString(s, " "))
}
