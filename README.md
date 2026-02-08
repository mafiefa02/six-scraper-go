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

### `GET /api/user`

Returns the authenticated student's ID and current semester.

**Response:**

```json
{
  "student_id": "10223085",
  "semester": "2025-2"
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
[
  {
    "code": "FI1210",
    "name": "Fisika Dasar",
    "sks": "3",
    "class_no": "01",
    "quota": "40/45",
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
]
```

## Caching

Schedule responses are cached in memory for 5 minutes. To force a fresh fetch, add `refresh=true` to the query string.

## Testing

```bash
go test -v ./...
```
