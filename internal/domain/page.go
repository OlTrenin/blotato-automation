package domain

import (
	"encoding/base64"
	"encoding/json"
	"time"
)

const MaxPageSize = 100

type Page struct {
	Limit int
	After string
}
type cursor struct {
	Time time.Time `json:"time"`
	ID   string    `json:"id"`
}

func (p Page) Values() (int, time.Time, string, error) {
	limit := p.Limit
	if limit == 0 {
		limit = MaxPageSize
	}
	if limit < 1 || limit > MaxPageSize {
		return 0, time.Time{}, "", ErrInvalid
	}
	if p.After == "" {
		return limit, time.Time{}, "", nil
	}
	if len(p.After) > 512 {
		return 0, time.Time{}, "", ErrInvalid
	}
	data, err := base64.RawURLEncoding.DecodeString(p.After)
	var c cursor
	if err != nil || json.Unmarshal(data, &c) != nil || !ValidIdentifier(c.ID) || c.Time.Year() < 1 || c.Time.Year() > 9999 {
		return 0, time.Time{}, "", ErrInvalid
	}
	return limit, c.Time, c.ID, nil
}

func NextCursor(t time.Time, id string) string {
	data, _ := json.Marshal(cursor{Time: t, ID: id})
	return base64.RawURLEncoding.EncodeToString(data)
}
