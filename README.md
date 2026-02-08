# six-scraper-go

A lightweight Go API that scrapes course schedule data from [SIX ITB](https://six.itb.ac.id) (Sistem Informasi Akademik ITB).

## Prerequisites

- Go 1.21+
- Valid SIX ITB session cookies (`nissin` and `khongguan`)

## Running

```bash
go run main.go
```

The server starts on `:8080`.

## API

All requests must include the `nissin` and `khongguan` authentication cookies.

All responses use a standard JSON envelope:

```json
// Success
{
  "success": true,
  "data": { ... }
}

// Error
{
  "success": false,
  "error": "descriptive error message"
}
```

### `GET /api/user`

Returns the authenticated student's ID and current semester.

**Response:**

```json
{
  "success": true,
  "data": {
    "student_id": "10223085",
    "semester": "2025-2"
  }
}
```

### `GET /api/schedule`

Returns the class schedule for a given student and semester.

**Required query parameters:**

| Parameter    | Description                   |
| ------------ | ----------------------------- |
| `student_id` | Student ID (from `/api/user`) |
| `semester`   | Semester code, e.g. `2025-2`  |

**Optional query parameters:**

| Parameter  | Description                   |
| ---------- | ----------------------------- |
| `fakultas` | Filter by faculty             |
| `prodi`    | Filter by program             |
| `pekan`    | Filter by week                |
| `kegiatan` | Filter by activity            |
| `refresh`  | Set to `true` to bypass cache |

**Example:**

```
GET /api/schedule?student_id=10223085&semester=2025-2
```

**Response:**

```json
{
  "success": true,
  "data": [
    {
      "code": "FI1210",
      "name": "Fisika Dasar",
      "sks": 3,
      "class_no": "01",
      "quota": 45,
      "lecturers": ["Dosen A", "Dosen B"],
      "notes": "",
      "schedules": [
        {
          "day": "Senin",
          "time": "07:00-09:00",
          "room": "7602",
          "activity": "Kuliah",
          "method": "Offline"
        }
      ]
    }
  ],
  "meta": {
    "fetched_at": "2025-02-08T12:34:56Z",
    "cached": false
  }
}
```

The `meta` field is included in schedule responses:
- `fetched_at` — when the data was last fetched from SIX
- `cached` — whether the response was served from cache

## Caching

Schedule responses are cached in memory for 5 minutes. To force a fresh fetch, add `refresh=true` to the query string.

## Testing

```bash
go test -v ./...
```
