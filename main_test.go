package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
)

func TestCollapseWhitespace(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"hello world", "hello world"},
		{"  hello   world  ", "hello world"},
		{"line1\nline2\n\nline3", "line1 line2 line3"},
		{"\t  tabs\tand  spaces  \n", "tabs and spaces"},
		{"", ""},
		{"   ", ""},
		{"single", "single"},
	}
	for _, tt := range tests {
		if got := collapseWhitespace(tt.input); got != tt.want {
			t.Errorf("collapseWhitespace(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBuildScheduleURL(t *testing.T) {
	t.Run("base only", func(t *testing.T) {
		q := url.Values{}
		q.Set("student_id", "10245001")
		q.Set("semester", "1945-1")
		got := buildScheduleURL("10245001", "1945-1", q)
		want := sixBaseURL + "/app/mahasiswa:10245001+1945-1/kelas/jadwal/kuliah"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("with optional params", func(t *testing.T) {
		q := url.Values{}
		q.Set("fakultas", "FMIPA")
		q.Set("prodi", "102")
		got := buildScheduleURL("10245001", "1945-1", q)
		if !strings.Contains(got, "fakultas=FMIPA") {
			t.Errorf("expected fakultas param in %q", got)
		}
		if !strings.Contains(got, "prodi=102") {
			t.Errorf("expected prodi param in %q", got)
		}
	})

	t.Run("ignores unknown params", func(t *testing.T) {
		q := url.Values{}
		q.Set("unknown", "value")
		got := buildScheduleURL("10245001", "1945-1", q)
		if strings.Contains(got, "unknown") {
			t.Errorf("unexpected param in %q", got)
		}
	})
}

func TestNewSIXRequest(t *testing.T) {
	t.Run("forwards cookies", func(t *testing.T) {
		incoming := httptest.NewRequest("GET", "/test", nil)
		incoming.AddCookie(&http.Cookie{Name: "nissin", Value: "abc"})
		incoming.AddCookie(&http.Cookie{Name: "khongguan", Value: "xyz"})

		req, err := newSIXRequest("https://example.com", incoming)
		if err != nil {
			t.Fatal(err)
		}

		for _, name := range []string{"nissin", "khongguan"} {
			c, err := req.Cookie(name)
			if err != nil {
				t.Errorf("missing cookie %q", name)
				continue
			}
			if name == "nissin" && c.Value != "abc" {
				t.Errorf("nissin = %q, want %q", c.Value, "abc")
			}
			if name == "khongguan" && c.Value != "xyz" {
				t.Errorf("khongguan = %q, want %q", c.Value, "xyz")
			}
		}

		if ua := req.Header.Get("User-Agent"); ua == "" {
			t.Error("expected User-Agent header to be set")
		}
	})

	t.Run("rejects missing nissin", func(t *testing.T) {
		incoming := httptest.NewRequest("GET", "/test", nil)
		incoming.AddCookie(&http.Cookie{Name: "khongguan", Value: "xyz"})

		_, err := newSIXRequest("https://example.com", incoming)
		if err == nil {
			t.Fatal("expected error for missing nissin cookie")
		}
		if !strings.Contains(err.Error(), "nissin") {
			t.Errorf("error should mention nissin: %v", err)
		}
	})

	t.Run("rejects missing khongguan", func(t *testing.T) {
		incoming := httptest.NewRequest("GET", "/test", nil)
		incoming.AddCookie(&http.Cookie{Name: "nissin", Value: "abc"})

		_, err := newSIXRequest("https://example.com", incoming)
		if err == nil {
			t.Fatal("expected error for missing khongguan cookie")
		}
		if !strings.Contains(err.Error(), "khongguan") {
			t.Errorf("error should mention khongguan: %v", err)
		}
	})

	t.Run("rejects no cookies", func(t *testing.T) {
		incoming := httptest.NewRequest("GET", "/test", nil)
		_, err := newSIXRequest("https://example.com", incoming)
		if err == nil {
			t.Fatal("expected error for missing cookies")
		}
	})
}

const testScheduleHTML = `<html><body>
<table class="table"><tbody>
<tr>
	<td>1</td>
	<td>check</td>
	<td>FI1210</td>
	<td>Fisika Dasar</td>
	<td>3</td>
	<td>01</td>
	<td>45</td>
	<td><ul><li>Dosen A</li><li>Dosen B</li></ul></td>
	<td>
		Catatan
		penting
	</td>
	<td>
		<ul>
			<li>Senin / 1945-01-06 / 07:00-09:00 / 7602 / Kuliah / Offline</li>
			<li>Rabu / 1945-01-08 / 13:00-15:00 / 7603 / Kuliah / Online</li>
		</ul>
	</td>
</tr>
<tr>
	<td>2</td>
	<td>check</td>
	<td>FI1220</td>
	<td>Fisika Lanjut</td>
	<td>3</td>
	<td>02</td>
	<td>40</td>
	<td><ul><li>Dosen C</li></ul></td>
	<td></td>
	<td>
		<ul>
			<li>Selasa / 1945-01-07 / 09:00-11:00 / 7604 / Kuliah / Offline</li>
		</ul>
	</td>
</tr>
</tbody></table>
</body></html>`

func docFromHTML(html string) *goquery.Document {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		panic(err)
	}
	return doc
}

func TestParseClasses(t *testing.T) {
	doc := docFromHTML(testScheduleHTML)
	classes := parseClasses(doc)

	if len(classes) != 2 {
		t.Fatalf("expected 2 classes, got %d", len(classes))
	}

	c := classes[0]
	if c.Code != "FI1210" {
		t.Errorf("Code = %q, want FI1210", c.Code)
	}
	if c.Name != "Fisika Dasar" {
		t.Errorf("Name = %q, want Fisika Dasar", c.Name)
	}
	if c.SKS != 3 {
		t.Errorf("SKS = %d, want 3", c.SKS)
	}
	if c.ClassNo != "01" {
		t.Errorf("ClassNo = %q, want 01", c.ClassNo)
	}
	if c.Quota != 45 {
		t.Errorf("Quota = %d, want 45", c.Quota)
	}
	if len(c.Lecturers) != 2 || c.Lecturers[0] != "Dosen A" || c.Lecturers[1] != "Dosen B" {
		t.Errorf("Lecturers = %v, want [Dosen A, Dosen B]", c.Lecturers)
	}
	if c.Notes != "Catatan penting" {
		t.Errorf("Notes = %q, want %q", c.Notes, "Catatan penting")
	}
	if len(c.Schedules) != 2 {
		t.Fatalf("expected 2 schedules, got %d", len(c.Schedules))
	}
	if c.Schedules[0].Day != "Senin" || c.Schedules[0].Time != "07:00-09:00" || c.Schedules[0].Room != "7602" {
		t.Errorf("Schedule[0] = %+v", c.Schedules[0])
	}
	if c.Schedules[1].Method != "Online" {
		t.Errorf("Schedule[1].Method = %q, want Online", c.Schedules[1].Method)
	}

	c2 := classes[1]
	if c2.Code != "FI1220" {
		t.Errorf("Second class Code = %q, want FI1220", c2.Code)
	}
	if len(c2.Lecturers) != 1 {
		t.Errorf("expected 1 lecturer for second class, got %d", len(c2.Lecturers))
	}
}

func TestParseClasses_SkipsRowsWithFewCells(t *testing.T) {
	html := `<html><body>
	<table class="table"><tbody>
		<tr><td>only</td><td>two</td></tr>
	</tbody></table>
	</body></html>`
	classes := parseClasses(docFromHTML(html))
	if len(classes) != 0 {
		t.Errorf("expected 0 classes, got %d", len(classes))
	}
}

func TestParseClasses_SkipsEmptyCode(t *testing.T) {
	html := `<html><body>
	<table class="table"><tbody>
	<tr>
		<td>1</td><td>x</td><td>  </td><td>Name</td><td>3</td>
		<td>01</td><td>40</td><td><ul></ul></td><td></td><td><ul></ul></td>
	</tr>
	</tbody></table>
	</body></html>`
	classes := parseClasses(docFromHTML(html))
	if len(classes) != 0 {
		t.Errorf("expected 0 classes for empty code, got %d", len(classes))
	}
}

func TestParseSchedules_Deduplication(t *testing.T) {
	html := `<ul>
		<li>Senin / 1945-01-06 / 07:00-09:00 / 7602 / Kuliah / Offline</li>
		<li>Senin / 1945-01-13 / 07:00-09:00 / 7602 / Kuliah / Offline</li>
	</ul>`
	doc := docFromHTML(html)
	sel := doc.Find("ul")
	schedules := parseSchedules(sel)
	if len(schedules) != 1 {
		t.Errorf("expected 1 deduplicated schedule, got %d", len(schedules))
	}
}

func TestParseSchedules_SkipsTampilkanSemua(t *testing.T) {
	html := `<ul>
		<li>Senin / 1945-01-06 / 07:00-09:00 / 7602 / Kuliah / Offline</li>
		<li>Tampilkan semua jadwal</li>
	</ul>`
	doc := docFromHTML(html)
	schedules := parseSchedules(doc.Find("ul"))
	if len(schedules) != 1 {
		t.Errorf("expected 1 schedule (Tampilkan semua skipped), got %d", len(schedules))
	}
}

func TestParseSchedules_SkipsInvalidFormat(t *testing.T) {
	html := `<ul>
		<li>invalid text without slashes</li>
		<li>only/three/parts</li>
	</ul>`
	doc := docFromHTML(html)
	schedules := parseSchedules(doc.Find("ul"))
	if len(schedules) != 0 {
		t.Errorf("expected 0 schedules for invalid format, got %d", len(schedules))
	}
}

func TestParseLecturers_Empty(t *testing.T) {
	html := `<div><ul></ul></div>`
	doc := docFromHTML(html)
	lecturers := parseLecturers(doc.Find("div"))
	if len(lecturers) != 0 {
		t.Errorf("expected 0 lecturers, got %d", len(lecturers))
	}
}

func clearCache() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	scheduleCache = make(map[string]cacheEntry)
}

func TestCache_SetAndGet(t *testing.T) {
	clearCache()
	data := []CourseClass{{Code: "FI1210", Name: "Test"}}
	now := time.Now()

	setCache("key1", data, now)

	entry, ok := getCached("key1")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if len(entry.data) != 1 || entry.data[0].Code != "FI1210" {
		t.Errorf("cached data mismatch: %+v", entry.data)
	}
	if !entry.fetchedAt.Equal(now) {
		t.Errorf("fetchedAt = %v, want %v", entry.fetchedAt, now)
	}
}

func TestCache_Miss(t *testing.T) {
	clearCache()
	_, ok := getCached("nonexistent")
	if ok {
		t.Error("expected cache miss")
	}
}

func TestCache_Expiry(t *testing.T) {
	clearCache()

	// Manually insert an expired entry
	cacheMu.Lock()
	scheduleCache["expired"] = cacheEntry{
		data:      []CourseClass{{Code: "OLD"}},
		expiresAt: time.Now().Add(-1 * time.Second),
	}
	cacheMu.Unlock()

	_, ok := getCached("expired")
	if ok {
		t.Error("expected cache miss for expired entry")
	}
}

// Creates a test server that mimics the SIX endpoints needed by the handlers.
func mockSIX(studentID, semester string) *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/home", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><body><a href="/app/mahasiswa:%s/home">Profile</a></body></html>`, studentID)
	})

	mux.HandleFunc("/app/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// Redirect from /app/mahasiswa:ID/kelas -> /app/mahasiswa:ID+SEMESTER/kelas/...
		if strings.HasSuffix(path, "/kelas") && !strings.Contains(path, "+") {
			dest := fmt.Sprintf("/app/mahasiswa:%s+%s/kelas/jadwal/kuliah", studentID, semester)
			http.Redirect(w, r, dest, http.StatusFound)
			return
		}
		// Serve schedule page
		fmt.Fprint(w, testScheduleHTML)
	})

	return httptest.NewServer(mux)
}

func addAuthCookies(r *http.Request) {
	r.AddCookie(&http.Cookie{Name: "nissin", Value: "test"})
	r.AddCookie(&http.Cookie{Name: "khongguan", Value: "test"})
}

func TestScheduleHandler_MissingParams(t *testing.T) {
	tests := []struct {
		name, query string
	}{
		{"missing both", ""},
		{"missing semester", "?student_id=123"},
		{"missing student_id", "?semester=1945-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/schedule"+tt.query, nil)
			addAuthCookies(req)
			w := httptest.NewRecorder()
			scheduleHandler(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
			}
			var resp APIResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatal(err)
			}
			if resp.Success {
				t.Error("expected success to be false")
			}
			if resp.Error == "" {
				t.Error("expected non-empty error message")
			}
		})
	}
}

func TestScheduleHandler_MissingCookies(t *testing.T) {
	clearCache()
	req := httptest.NewRequest("GET", "/api/schedule?student_id=123&semester=1945-1", nil)
	w := httptest.NewRecorder()
	scheduleHandler(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("got status %d, want %d", w.Code, http.StatusBadGateway)
	}
	var resp APIResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Success {
		t.Error("expected success to be false")
	}
	if resp.Error == "" {
		t.Error("expected non-empty error message")
	}
}

func TestScheduleHandler_CacheHit(t *testing.T) {
	clearCache()

	cached := []CourseClass{{Code: "CACHED01", Name: "From Cache"}}
	key := buildScheduleURL("123", "1945-1", url.Values{})
	setCache(key, cached, time.Now())

	req := httptest.NewRequest("GET", "/api/schedule?student_id=123&semester=1945-1", nil)
	addAuthCookies(req)
	w := httptest.NewRecorder()
	scheduleHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", w.Code)
	}

	var resp APIResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Success {
		t.Error("expected success to be true")
	}
	if resp.Meta == nil {
		t.Fatal("expected meta to be present")
	}
	if !resp.Meta.Cached {
		t.Error("expected meta.cached to be true")
	}

	// Decode data as []CourseClass
	dataBytes, err := json.Marshal(resp.Data)
	if err != nil {
		t.Fatal(err)
	}
	var classes []CourseClass
	if err := json.Unmarshal(dataBytes, &classes); err != nil {
		t.Fatal(err)
	}
	if len(classes) != 1 || classes[0].Code != "CACHED01" {
		t.Errorf("expected cached response, got %+v", classes)
	}
}

func TestScheduleHandler_RefreshBypassesCache(t *testing.T) {
	clearCache()

	cached := []CourseClass{{Code: "STALE", Name: "Stale Data"}}
	key := buildScheduleURL("123", "1945-1", url.Values{})
	setCache(key, cached, time.Now())

	// With refresh=true, the handler should not return the cached data.
	// It will try to fetch from upstream (which won't work without a real server),
	// so we expect a bad gateway rather than the cached response.
	req := httptest.NewRequest("GET", "/api/schedule?student_id=123&semester=1945-1&refresh=true", nil)
	addAuthCookies(req)
	w := httptest.NewRecorder()
	scheduleHandler(w, req)

	// Should not have returned 200 with stale data
	if w.Code == http.StatusOK {
		var resp APIResponse
		json.NewDecoder(w.Body).Decode(&resp)
		dataBytes, _ := json.Marshal(resp.Data)
		var classes []CourseClass
		json.Unmarshal(dataBytes, &classes)
		if len(classes) == 1 && classes[0].Code == "STALE" {
			t.Error("refresh=true should bypass cache, but got stale cached data")
		}
	}
}

func TestUserHandler_MissingCookies(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/user", nil)
	w := httptest.NewRecorder()
	userHandler(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("got status %d, want %d", w.Code, http.StatusBadGateway)
	}
	var resp APIResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Success {
		t.Error("expected success to be false")
	}
	if resp.Error == "" {
		t.Error("expected non-empty error message")
	}
}
