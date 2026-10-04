package players

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// SteamInfo is what Steam says about an account.
type SteamInfo struct {
	Avatar           string     `json:"avatar"`
	ProfileURL       string     `json:"profile_url"`
	AccountCreated   *time.Time `json:"account_created"`
	VACBans          int        `json:"vac_bans"`
	GameBans         int        `json:"game_bans"`
	DaysSinceLastBan *int       `json:"days_since_last_ban"`
	CommunityBanned  bool       `json:"community_banned"`
}

const (
	steamBatch     = 100
	steamTimeout   = 5 * time.Second
	steamHit       = 24 * time.Hour
	steamMiss      = 15 * time.Minute
	steamCacheSize = 20000
	steamMaxBody   = 4 << 20

	// A long-standing public account, for checking a key.
	steamProbeID = "76561197960287930"
)

// Why a key check failed.
var (
	ErrSteamKeyRejected = errors.New("steam rejected the key")
	ErrSteamUnreachable = errors.New("steam could not be reached")
)

var steamKeyShape = regexp.MustCompile(`^[0-9A-Fa-f]{32}$`)

// ValidSteamKey says whether s has the shape of a Steam Web API key.
func ValidSteamKey(s string) bool { return steamKeyShape.MatchString(s) }

// IsSteamID64 says whether id is a 17-digit SteamID.
func IsSteamID64(id string) bool {
	if len(id) != 17 {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

type steamEntry struct {
	info    *SteamInfo // nil when Steam had nothing
	expires time.Time
}

// Steam looks up accounts through the Steam Web API. Answers are kept for
// a day and failures for a quarter of an hour, so a page that is opened
// again and again asks Steam once.
type Steam struct {
	// BaseURL is Steam's API; empty means the real one.
	BaseURL string
	HTTP    *http.Client
	Now     func() time.Time

	mu     sync.Mutex
	cache  map[string]steamEntry
	flight map[string]chan struct{}
}

func (s *Steam) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Steam) base() string {
	if s.BaseURL != "" {
		return strings.TrimRight(s.BaseURL, "/")
	}
	return "https://api.steampowered.com"
}

func (s *Steam) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return http.DefaultClient
}

// get fetches one API call and decodes it. The key is in the address, so
// errors from the HTTP client are cut down to their cause.
func (s *Steam) get(ctx context.Context, path, key string, ids []string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, steamTimeout)
	defer cancel()
	q := url.Values{"key": {key}, "steamids": {strings.Join(ids, ",")}}
	req, err := http.NewRequestWithContext(ctx, "GET", s.base()+path+"?"+q.Encode(), nil)
	if err != nil {
		return ErrSteamUnreachable
	}
	resp, err := s.client().Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("%w: %v", ErrSteamUnreachable, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrSteamKeyRejected
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%w: status %d", ErrSteamUnreachable, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, steamMaxBody)).Decode(out); err != nil {
		return fmt.Errorf("%w: unreadable answer", ErrSteamUnreachable)
	}
	return nil
}

type steamSummaries struct {
	Response struct {
		Players []struct {
			SteamID     string `json:"steamid"`
			AvatarFull  string `json:"avatarfull"`
			ProfileURL  string `json:"profileurl"`
			TimeCreated int64  `json:"timecreated"`
		} `json:"players"`
	} `json:"response"`
}

type steamBans struct {
	Players []struct {
		SteamID          string `json:"SteamId"`
		CommunityBanned  bool   `json:"CommunityBanned"`
		NumberOfVACBans  int    `json:"NumberOfVACBans"`
		DaysSinceLastBan int    `json:"DaysSinceLastBan"`
		NumberOfGameBans int    `json:"NumberOfGameBans"`
	} `json:"players"`
}

// Check asks Steam about one known account to see that the key works.
func (s *Steam) Check(ctx context.Context, key string) error {
	var out steamSummaries
	return s.get(ctx, "/ISteamUser/GetPlayerSummaries/v2/", key, []string{steamProbeID}, &out)
}

// fetch asks for up to steamBatch accounts.
func (s *Steam) fetch(ctx context.Context, key string, ids []string) (map[string]*SteamInfo, error) {
	var sum steamSummaries
	if err := s.get(ctx, "/ISteamUser/GetPlayerSummaries/v2/", key, ids, &sum); err != nil {
		return nil, err
	}
	var bans steamBans
	if err := s.get(ctx, "/ISteamUser/GetPlayerBans/v1/", key, ids, &bans); err != nil {
		return nil, err
	}
	out := map[string]*SteamInfo{}
	for _, p := range sum.Response.Players {
		info := &SteamInfo{Avatar: p.AvatarFull, ProfileURL: p.ProfileURL}
		if p.TimeCreated > 0 {
			t := time.Unix(p.TimeCreated, 0)
			info.AccountCreated = &t
		}
		out[p.SteamID] = info
	}
	for _, b := range bans.Players {
		info := out[b.SteamID]
		if info == nil {
			info = &SteamInfo{}
			out[b.SteamID] = info
		}
		info.VACBans, info.GameBans, info.CommunityBanned = b.NumberOfVACBans, b.NumberOfGameBans, b.CommunityBanned
		if b.NumberOfVACBans+b.NumberOfGameBans > 0 {
			d := b.DaysSinceLastBan
			info.DaysSinceLastBan = &d
		}
	}
	return out, nil
}

// Lookup returns what is known of the 17-digit ids that Steam answered
// for. Others, and everything while Steam fails, are left out.
func (s *Steam) Lookup(ctx context.Context, key string, ids []string) map[string]*SteamInfo {
	out := map[string]*SteamInfo{}
	var want []string
	seen := map[string]bool{}
	for _, id := range ids {
		if IsSteamID64(id) && !seen[id] {
			seen[id] = true
			want = append(want, id)
		}
	}
	for len(want) > 0 {
		var own, wait []string
		var waitOn []chan struct{}
		s.mu.Lock()
		now := s.now()
		for _, id := range want {
			if e, ok := s.cache[id]; ok && now.Before(e.expires) {
				if e.info != nil {
					out[id] = e.info
				}
				continue
			}
			if ch, ok := s.flight[id]; ok {
				wait, waitOn = append(wait, id), append(waitOn, ch)
				continue
			}
			if s.flight == nil {
				s.flight = map[string]chan struct{}{}
			}
			s.flight[id] = make(chan struct{})
			own = append(own, id)
		}
		s.mu.Unlock()

		for len(own) > 0 {
			batch := own[:min(len(own), steamBatch)]
			own = own[len(batch):]
			got, err := s.fetch(ctx, key, batch)
			s.mu.Lock()
			s.trim()
			exp := s.now().Add(steamHit)
			if err != nil {
				exp = s.now().Add(steamMiss)
			}
			if s.cache == nil {
				s.cache = map[string]steamEntry{}
			}
			for _, id := range batch {
				// A caller that gave up says nothing about Steam.
				if err == nil || ctx.Err() == nil {
					s.cache[id] = steamEntry{info: got[id], expires: exp}
				}
				if got[id] != nil {
					out[id] = got[id]
				}
				close(s.flight[id])
				delete(s.flight, id)
			}
			s.mu.Unlock()
		}
		// Whoever else asked for these is done, or we give up on them.
		for _, ch := range waitOn {
			select {
			case <-ch:
			case <-ctx.Done():
				return out
			}
		}
		// The waited-for ids are in the cache now; one more pass reads them.
		// An id that is still missing then has its own failure cached.
		want = wait
		if len(wait) > 0 {
			var again []string
			s.mu.Lock()
			for _, id := range wait {
				if e, ok := s.cache[id]; ok && s.now().Before(e.expires) {
					if e.info != nil {
						out[id] = e.info
					}
				} else {
					again = append(again, id)
				}
			}
			s.mu.Unlock()
			want = again
		}
	}
	return out
}

// trim keeps the cache bounded. The caller holds mu.
func (s *Steam) trim() {
	if len(s.cache) < steamCacheSize {
		return
	}
	now := s.now()
	for id, e := range s.cache {
		if !now.Before(e.expires) {
			delete(s.cache, id)
		}
	}
	// Still full of live entries: drop an arbitrary tenth.
	drop := len(s.cache) - steamCacheSize + steamCacheSize/10
	for id := range s.cache {
		if drop <= 0 {
			break
		}
		delete(s.cache, id)
		drop--
	}
}

// Forget empties the cache, for when the key changes.
func (s *Steam) Forget() {
	s.mu.Lock()
	s.cache = nil
	s.mu.Unlock()
}
