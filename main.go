package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
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
	SKS       string          `json:"sks"`
	ClassNo   string          `json:"class_no"`
	Quota     string          `json:"quota"`
	Lecturers []string        `json:"lecturers"`
	Notes     string          `json:"notes"`
	Schedules []ScheduleEntry `json:"schedules"`
}

type UserResponse struct {
	StudentID string `json:"student_id"`
	Semester  string `json:"semester"`
}

var requiredCookies = []string{"nissin", "khongguan"}

const cacheTTL = 5 * time.Minute

type cacheEntry struct {
	data      []CourseClass
	expiresAt time.Time
}

var (
	scheduleCache = make(map[string]cacheEntry)
	cacheMu       sync.RWMutex
)

func main() {
	http.HandleFunc("/api/user", userHandler)
	http.HandleFunc("/api/schedule", scheduleHandler)

	fmt.Println("Server starting on :8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
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

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, resp, fmt.Errorf("upstream returned %s", resp.Status)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, resp, err
	}
	return doc, resp, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
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
		http.Error(w, err.Error(), http.StatusBadGateway)
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
		http.Error(w, "Could not find student ID on /home", http.StatusNotFound)
		return
	}

	// Get Semester from redirect URL
	redirectURL := fmt.Sprintf("%s/app/mahasiswa:%s/kelas", sixBaseURL, studentID)
	req, err := newSIXRequest(redirectURL, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp.Body.Close()

	finalURL := resp.Request.URL.String()
	m := semesterRe.FindStringSubmatch(finalURL)
	if len(m) < 2 {
		http.Error(w, "Could not infer semester from redirect URL: "+finalURL, http.StatusNotFound)
		return
	}

	writeJSON(w, UserResponse{StudentID: studentID, Semester: m[1]})
}

func scheduleHandler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	studentID := query.Get("student_id")
	semester := query.Get("semester")

	if studentID == "" || semester == "" {
		http.Error(w, "Missing student_id or semester query parameters", http.StatusBadRequest)
		return
	}

	targetURL := buildScheduleURL(studentID, semester, query)
	refresh := query.Get("refresh") == "true"

	if !refresh {
		if classes, ok := getCached(targetURL); ok {
			writeJSON(w, classes)
			return
		}
	}

	client := newHTTPClient()
	doc, _, err := fetchDoc(client, targetURL, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	classes := parseClasses(doc)
	setCache(targetURL, classes)
	writeJSON(w, classes)
}

func getCached(key string) ([]CourseClass, bool) {
	cacheMu.RLock()
	defer cacheMu.RUnlock()
	entry, ok := scheduleCache[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return nil, false
	}
	return entry.data, true
}

func setCache(key string, data []CourseClass) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	scheduleCache[key] = cacheEntry{data: data, expiresAt: time.Now().Add(cacheTTL)}
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

		class := CourseClass{
			Code:      strings.TrimSpace(cells.Eq(2).Text()),
			Name:      strings.TrimSpace(cells.Eq(3).Text()),
			SKS:       strings.TrimSpace(cells.Eq(4).Text()),
			ClassNo:   strings.TrimSpace(cells.Eq(5).Text()),
			Quota:     strings.TrimSpace(cells.Eq(6).Text()),
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
