package api

import "context"

// Backend is the seam between the HTTP/WS layer and the domain; api never imports the domain.
type Backend interface {
	Companions() []CompanionInfo

	// SPIBoards lists the radio hats this build knows how to wire.
	SPIBoards() []SPIBoardInfo

	// RadioStats reports the modem's link counters; on the SPI path they are the radio's only health
	// signal. False means there is no modem to report on, which must not be answered with zeroes.
	RadioStats() (RadioStatsInfo, bool)

	// Health reports the monitoring snapshot behind GET /api/health.
	Health() HealthInfo

	// ResetModem drops the modem and reconnects it, the same path a vanished serial port takes.
	ResetModem()

	// StartDiscovery broadcasts a zero-hop discovery for the given firmware ADV_TYPEs. False means
	// there is no node to send it from, which is a different answer from finding nothing.
	StartDiscovery(types []int) (DiscoveryState, bool)
	// DiscoveryState reports the current scan without starting one.
	DiscoveryState() (DiscoveryState, bool)

	// ChannelByHash resolves a channel hash byte across every companion; nil when unknown.
	ChannelByHash(hash byte) *ChannelInfo

	// AddPeer registers a peer in every companion's in-memory table, so it is reachable before its advert.
	AddPeer(pubkey []byte, name, peerType string)

	// RemovePeers drops peers from every companion's in-memory table; the DB row is deleted separately.
	RemovePeers(pubkeys [][]byte)

	// Companion returns the channel/DM senders for a named companion.
	Companion(name string) (MessageSender, DMSender, bool)

	ChannelMutator(name string) (adder ChannelAdder, remover ChannelRemover, ok bool)

	RenameChannel(companionName, oldName, newName string) error

	TraceSender(name string) (TraceSender, bool)

	AdvertSender(name string) (AdvertSender, bool)

	Repeater(name string) (*RepeaterOps, bool)

	// MqttStatus reports each broker's live state; ok=false when no MQTT observer is running.
	MqttStatus() ([]MqttBrokerStatus, bool)

	// ExportBackup builds a downloadable backup honouring opts.
	ExportBackup(ctx context.Context, opts BackupOptions) (*BackupFile, error)
	// EstimateBackup reports what opts would capture, without building the file.
	EstimateBackup(ctx context.Context, opts BackupOptions) (*BackupEstimate, error)
	// ImportBackup restores a backup database or applies a config file; filename only picks the config parser.
	ImportBackup(ctx context.Context, data []byte, filename string) (*ImportResult, error)

	// RepeaterNode returns ops for the repeater we run, not a remote one we drive; ok=false when none runs.
	RepeaterNode() (*RepeaterNodeOps, bool)

	// PersistChannels writes the companions' current channels back to the config file.
	PersistChannels(ctx context.Context) error

	// Config writes by surrogate id: id==0 creates and returns it, id>0 updates; *string secrets are nil=keep, ""=clear.
	SaveSettings(ctx context.Context, in SettingsInput) error
	SaveMqtt(ctx context.Context, in MqttInput) error
	SaveBroker(ctx context.Context, in BrokerInput) (int64, error)
	DeleteBroker(ctx context.Context, id int64) error
	SaveCompanion(ctx context.Context, in CompanionInput) (int64, error)
	DeleteCompanion(ctx context.Context, id int64) error
	SaveChannel(ctx context.Context, in ChannelInput) (int64, error)
	DeleteChannel(ctx context.Context, id int64) error
	SaveTrigger(ctx context.Context, in TriggerInput) (int64, error)
	DeleteTrigger(ctx context.Context, id int64) error

	// Repeater config is per-section; CreateRepeater generates a key when PrivateKey is nil and seeds the "*" region.
	CreateRepeater(ctx context.Context, in RepeaterCreateInput) error
	UpdateRepeaterNode(ctx context.Context, in RepeaterNodeInput) error
	UpdateRepeaterRelay(ctx context.Context, in RepeaterRelayInput) error
	UpdateRepeaterAdmin(ctx context.Context, in RepeaterAdminInput) error
	AddRepeaterRegion(ctx context.Context, in RepeaterRegionInput) error
	SetRepeaterRegionFlood(ctx context.Context, name string, denyFlood bool) error
	RemoveRepeaterRegion(ctx context.Context, name string) error
	DeleteRepeater(ctx context.Context) error
}

// RepeaterNodeOps are runtime operations on the running repeater node.
type RepeaterNodeOps struct {
	Name       string
	Stats      func() any // live relay counters + uptime + neighbour count
	Neighbors  func() any // directly-heard repeaters
	Advert     func(flood bool) error
	Discover   func() error                         // zero-hop NODE_DISCOVER_REQ; responses land in Neighbors
	ACL        func() any                           // admin clients in the ACL (name-resolved)
	RevokeACL  func(pubkey string) error            // drop a client's access
	SetACL     func(pubkey string, perms int) error // grant / change a client's role (setperm)
	ClearStats func()                               // reset relay counters (clear stats)
}

// MqttBrokerStatus mirrors mqtt.BrokerStatus; duplicated because api must not import the domain.
type MqttBrokerStatus struct {
	Name        string `json:"name"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Transport   string `json:"transport"`
	TLS         bool   `json:"tls"`
	AuthType    string `json:"authType"`
	Enabled     bool   `json:"enabled"`
	Connected   bool   `json:"connected"`
	LastError   string `json:"lastError,omitempty"`
	LastErrorTs int64  `json:"lastErrorTs,omitempty"`
	ConnectedTs int64  `json:"connectedTs,omitempty"`
	Published   uint64 `json:"published"`
	Dropped     uint64 `json:"dropped"`
	StatusTopic string `json:"statusTopic,omitempty"`
}

// --- per-resource config write inputs (JSON request bodies) ---

// SPIBoardInfo describes one radio hat this build knows about.
type SPIBoardInfo struct {
	Name       string `json:"name"`
	Label      string `json:"label"`
	Chip       string `json:"chip"`
	SPIPort    string `json:"spiPort"`
	MaxTxPower int    `json:"maxTxPower"`
	// Verified is "hardware" or "community"; shown in the picker before an antenna goes up.
	Verified string `json:"verified"`
	Notes    string `json:"notes,omitempty"`
	// Unsupported is why this build refuses the board, empty when it can drive it.
	Unsupported string `json:"unsupported,omitempty"`
	HasLEDs     bool   `json:"hasLeds"`
}

// DiscoveryInfo is one node that answered a zero-hop discovery.
type DiscoveryInfo struct {
	PubKey string `json:"pubkey"`
	// Name is resolved from peers we have already heard advert; empty for a node we only know by key.
	Name string `json:"name"`
	Type int    `json:"type"`
	// SNR is how well we heard them; ReportedSNR is how well they heard us, so an asymmetric link is visible.
	SNR         float64 `json:"snr"`
	ReportedSNR float64 `json:"reportedSnr"`
	// Heard is when this node answered, as a timestamp so the page can age it without polling.
	Heard string `json:"heard"`
}

// DiscoveryState is a scan and whatever has answered so far.
type DiscoveryState struct {
	Running bool `json:"running"`
	// SecsLeft counts down the collection window; responders answer after a random delay, so an
	// empty list before it reaches 0 means "not yet", not "nothing there".
	SecsLeft int `json:"secsLeft"`
	// ScanStartedAt is empty until a scan has run, so results are never shown without saying how old
	// they are: a finished scan's table looks identical to a live one.
	ScanStartedAt string          `json:"scanStartedAt"`
	Results       []DiscoveryInfo `json:"results"`
}

// HealthInfo backs GET /api/health: facts for a monitor to threshold, never verdicts, and 200 whenever the process is alive.
type HealthInfo struct {
	Status     string   `json:"status"` // "ok" when Problems is empty, else "degraded"
	Problems   []string `json:"problems"`
	Version    string   `json:"version"`
	UptimeSecs int64    `json:"uptimeSecs"`

	Radio    RadioHealth    `json:"radio"`
	Database DatabaseHealth `json:"database"`
	Brokers  []BrokerHealth `json:"brokers"`
}

// RadioHealth keeps "modem attached", "board answering" and "mesh talking" apart: a quiet mesh moves only the last.
type RadioHealth struct {
	Connected bool   `json:"connected"`
	Transport string `json:"transport"`

	// Null where this transport cannot be probed, so "not answering" and "cannot say" stay distinct.
	LastReplySecs *int64 `json:"lastReplySecs"`
	// Null when none since startup.
	LastRxSecs *int64 `json:"lastRxSecs"`
	LastTxSecs *int64 `json:"lastTxSecs"`

	TxQueueLen     int    `json:"txQueueLen"`
	TxSent         uint64 `json:"txSent"`
	TxFailed       uint64 `json:"txFailed"`
	TxDroppedBusy  uint64 `json:"txDroppedBusy"`
	TxDroppedQueue uint64 `json:"txDroppedQueue"`

	InboundDroppedNew uint64 `json:"inboundDroppedNew"`
	HandlerSlow       uint64 `json:"handlerSlow"`

	// Absent where the transport cannot measure them at all, 0 where it measured none.
	CRCErrors    *uint64 `json:"crcErrors,omitempty"`
	RecvErrors   *uint64 `json:"recvErrors,omitempty"`
	DriverErrors *uint64 `json:"driverErrors,omitempty"`
	HwErrors     *uint64 `json:"hwErrors,omitempty"`

	NoiseFloor *int16   `json:"noiseFloor,omitempty"`
	BatteryMV  *uint16  `json:"batteryMv,omitempty"`
	MCUTempC   *float64 `json:"mcuTempC,omitempty"`
}

// DatabaseHealth watches the write queue, not a ping: a dropped write leaves everything downstream looking healthy.
type DatabaseHealth struct {
	// A point sample of a queue that drains in microseconds: shows sustained backpressure, not a spike.
	WriteQueueLen int    `json:"writeQueueLen"`
	WriteQueueCap int    `json:"writeQueueCap"`
	WritesDropped uint64 `json:"writesDropped"`
	// The one to threshold: WritesDropped only rises, so it cannot tell "now" from "last Tuesday".
	WritesDroppedLastSecs *int64 `json:"writesDroppedLastSecs"`
}

// No node listing on purpose: a name or peer count cannot report a fault (a node that fails to start exits the process), and both resolve to coordinates on public maps.

// BrokerHealth carries ages, not the transport error (it names the broker's address) and not flags (the observer never clears lastErr, so a bool would latch).
type BrokerHealth struct {
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Connected bool   `json:"connected"`
	// Null when disconnected. Resetting toward zero on every scrape is how flapping shows; Connected alone reads true.
	ConnectedSecs *int64 `json:"connectedSecs"`
	// Null when it has never erred.
	LastErrorSecs *int64 `json:"lastErrorSecs"`
	Published     uint64 `json:"published"`
	Dropped       uint64 `json:"dropped"`
}

// RadioStatsInfo mirrors modem.LinkStats plus the radio's configuration; the pointer counters are absent when the backend cannot measure them, 0 when it measured none.
type RadioStatsInfo struct {
	FreqHz  uint32 `json:"freqHz"`
	BwHz    uint32 `json:"bwHz"`
	SF      uint8  `json:"sf"`
	CR      uint8  `json:"cr"`
	TxPower uint8  `json:"txPower"`

	InboundDroppedNew uint64 `json:"inboundDroppedNew"`
	HandlerSlow       uint64 `json:"handlerSlow"`

	HwDecodeErrors       *uint64 `json:"hwDecodeErrors,omitempty"`
	InboundDroppedOldest *uint64 `json:"inboundDroppedOldest,omitempty"`
	RxMetaTimeouts       *uint64 `json:"rxMetaTimeouts,omitempty"`
	RxMetaMisattributed  *uint64 `json:"rxMetaMisattributed,omitempty"`
	HwErrors             *uint64 `json:"hwErrors,omitempty"`
	TxOutcomeLost        *uint64 `json:"txOutcomeLost,omitempty"`

	PacketsRecv    *uint64 `json:"packetsRecv,omitempty"`
	PacketsSent    *uint64 `json:"packetsSent,omitempty"`
	CRCErrors      *uint64 `json:"crcErrors,omitempty"`
	RecvErrors     *uint64 `json:"recvErrors,omitempty"`
	DriverErrors   *uint64 `json:"driverErrors,omitempty"`
	RecvRecoveries *uint64 `json:"recvRecoveries,omitempty"`

	// Transport is "kiss" or "spi": which counters above can move at all depends on it.
	Transport string `json:"transport"`

	// TX outcomes, from the mux's own queue rather than the modem.
	TxSent         uint64 `json:"txSent"`
	TxFailed       uint64 `json:"txFailed"`
	TxRequeued     uint64 `json:"txRequeued"`
	TxDroppedBusy  uint64 `json:"txDroppedBusy"`
	TxDroppedQueue uint64 `json:"txDroppedQueue"`
	TxQueueLen     int    `json:"txQueueLen"`

	// Board readings, absent when this hardware has no such sensor or has stopped answering.
	UptimeSecs uint32   `json:"uptimeSecs"`
	NoiseFloor *int16   `json:"noiseFloor,omitempty"`
	BatteryMV  *uint16  `json:"batteryMv,omitempty"`
	MCUTempC   *float64 `json:"mcuTempC,omitempty"`
}

type SettingsInput struct {
	LogLevel       *string `json:"logLevel"`
	ConnectionType *string `json:"connectionType"`
	Connection     *string `json:"connection"`
	BaudRate       *int    `json:"baudRate"`
	// SPIBoard names the hat for an spi:// connection; omitted keeps the stored value.
	SPIBoard     *string  `json:"spiBoard"`
	Freq         *float64 `json:"freq"`
	BW           *float64 `json:"bw"`
	SF           *int     `json:"sf"`
	CR           *int     `json:"cr"`
	TX           *int     `json:"tx"`
	ListenAddr   *string  `json:"listenAddr"`
	MapTileKey   *string  `json:"mapTileKey"` // omit = keep, "" = clear
	// ModemToken is the openHop modem's access token: omit = keep the stored one, "" = clear it.
	ModemToken *string `json:"modemToken"`
	PathHashSize *int     `json:"pathHashSize"`
	// DutyCycle is a TX airtime cap percentage (0 < pct <= 100); null means the default, not "keep".
	DutyCycle     *float64 `json:"dutyCycle"`
	SetupComplete *bool    `json:"setupComplete"`
}

type MqttInput struct {
	Enabled         *bool   `json:"enabled"`
	NodeCompanionID *int64  `json:"nodeCompanionId"`
	IataCode        *string `json:"iataCode"`
	StatusInterval  *int    `json:"statusInterval"`
	Owner           *string `json:"owner"`
	Email           *string `json:"email"`
}

type BrokerInput struct {
	ID                    int64    `json:"id"`
	Name                  string   `json:"name"`
	Enabled               bool     `json:"enabled"`
	Dedup                 bool     `json:"dedup"`
	Transport             string   `json:"transport"`
	Host                  string   `json:"host"`
	Port                  int      `json:"port"`
	PacketTopic           *string  `json:"packetTopic"`
	StatusTopic           *string  `json:"statusTopic"`
	DisallowedPacketTypes []string `json:"disallowedPacketTypes"`
	RetainStatus          bool     `json:"retainStatus"`
	TLSEnabled            bool     `json:"tlsEnabled"`
	TLSInsecure           bool     `json:"tlsInsecure"`
	AuthType              string   `json:"authType"`
	Username              string   `json:"username"`
	Password              *string  `json:"password"` // nil = keep existing
	Path                  string   `json:"path"`
	Audience              string   `json:"audience"`
}

type CompanionInput struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	PrivateKey     *string  `json:"privateKey"` // nil = keep (update) / generate (create)
	DMPolicy       string   `json:"dmPolicy"`   // "" = contacts
	DMAllow        []string `json:"dmAllow"`
	Latitude       *float64 `json:"latitude"`
	Longitude      *float64 `json:"longitude"`
	AdvertInterval *int     `json:"advertInterval"`
	PathHashSize   *int     `json:"pathHashSize"`
}

type ChannelInput struct {
	ID          int64   `json:"id"`
	CompanionID int64   `json:"companionId"`
	Name        string  `json:"name"`
	PrivateKey  *string `json:"privateKey"` // nil = keep existing
}

// RepeaterCreateInput sets up the singleton; everything else is edited through the section endpoints.
type RepeaterCreateInput struct {
	Name       string  `json:"name"`
	PrivateKey *string `json:"privateKey"` // nil/empty = generate
	// AdminPassword is required and must be non-blank: a blank one compares equal to the blank a
	// login sends, which granted admin to any node in range.
	AdminPassword string `json:"adminPassword"`
}

// RepeaterNodeInput is the Node section; PrivateKey nil keeps the identity, a value rotates it.
type RepeaterNodeInput struct {
	Name       string   `json:"name"`
	PrivateKey *string  `json:"privateKey"`
	Latitude   *float64 `json:"latitude"`
	Longitude  *float64 `json:"longitude"`
}

// RepeaterRelayInput is the Relay-policy section: forwarding + advert cadence.
type RepeaterRelayInput struct {
	DisableFwd          *bool    `json:"disableFwd"`
	FloodMax            *int     `json:"floodMax"`
	FloodMaxUnscoped    *int     `json:"floodMaxUnscoped"`
	FloodMaxAdvert      *int     `json:"floodMaxAdvert"`
	LoopDetect          *string  `json:"loopDetect"`
	PathHashSize        *int     `json:"pathHashSize"`
	TxDelayFactor       *float64 `json:"txDelayFactor"`
	DirectTxDelayFactor *float64 `json:"directTxDelayFactor"`
	RxDelayBase         *float64 `json:"rxDelayBase"`
	MultiAcks           *int     `json:"multiAcks"`
	DefaultRegion       string   `json:"defaultRegion"` // "" = unscoped flood adverts
	AdvertInterval      *int     `json:"advertInterval"`
	FloodAdvertInterval *int     `json:"floodAdvertInterval"`
}

// RepeaterAdminInput is the Owner & access section; passwords are nil=keep, ""=clear.
type RepeaterAdminInput struct {
	OwnerInfo     string  `json:"ownerInfo"`
	AdminPassword *string `json:"adminPassword"`
	GuestPassword *string `json:"guestPassword"`
}

// RepeaterRegionInput is a region add (POST) or deny-flood toggle (PATCH) body.
type RepeaterRegionInput struct {
	Name      string `json:"name"`
	DenyFlood bool   `json:"denyFlood"`
}

type TriggerInput struct {
	ID                 int64    `json:"id"`
	CompanionID        int64    `json:"companionId"`
	Type               string   `json:"type"`
	Template           string   `json:"template"`
	CharLimitBehaviour *string  `json:"charLimitBehaviour"`
	Match              []string `json:"match"`
	Contacts           []string `json:"contacts"`
	ChannelIDs         []int64  `json:"channelIds"`
	FailoverPattern    string   `json:"failoverPattern"`
	FailoverTimeout    int64    `json:"failoverTimeout"`
	RetryTimeout       *int64   `json:"retryTimeout"`
	MaxRetries         *int     `json:"maxRetries"`
	PathHashSize       *int     `json:"pathHashSize"`
	Schedule           *string  `json:"schedule"`
	URL                string   `json:"url"`
}

// BackupFile is a generated backup ready to stream to the browser.
type BackupFile struct {
	Name        string
	ContentType string
	Data        []byte
}

// BackupOptions day fields are -1 for everything, 0 for none, or a positive number of days back.
type BackupOptions struct {
	// CompanionIDs is required: [] is none, and a pointer only so an omitted field is rejected.
	CompanionIDs *[]int64 `json:"companionIds"`
	Contacts     bool     `json:"contacts"`
	Triggers     bool     `json:"triggers"`
	Mqtt         bool     `json:"mqtt"`
	Repeater     bool     `json:"repeater"`
	Peers        bool     `json:"peers"`
	MessageDays  int      `json:"messageDays"`
	PacketDays   int      `json:"packetDays"`
	MetricDays   int      `json:"metricDays"`
	// IdentityKeys keeps the node private keys, so a restore is the same node on the mesh.
	IdentityKeys bool `json:"identityKeys"`
}

// BackupEstimate is the row count a selection captures, plus the DB size as an upper bound.
type BackupEstimate struct {
	Companions int64 `json:"companions"`
	Contacts   int64 `json:"contacts"`
	Messages   int64 `json:"messages"`
	Packets    int64 `json:"packets"`
	Peers      int64 `json:"peers"`
	Metrics    int64 `json:"metrics"`
	Bytes      int64 `json:"bytes"`
}

// ImportResult describes what a restore did.
type ImportResult struct {
	// Kind is "database" (a backup, staged for restart) or "config".
	Kind            string `json:"kind"`
	Companions      int    `json:"companions"`
	RestartRequired bool   `json:"restartRequired"`
	SchemaVersion   int    `json:"schemaVersion,omitempty"`
	Detail          string `json:"detail"`
}
