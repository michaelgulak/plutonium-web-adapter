package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"math/big"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	supportBankNotes = false
	skillCount       = 18
	readWriteTimeout = 10
	connectTimeout   = 30
	maxPacketSize    = 30000
	botMessagesSize  = 100
	botVersion       = "1.12.7"
)

// Gap between logins across all accounts, so the server isn't spammed.
// A variable so tests can shorten it.
var loginStagger = 6 * time.Second

var (
	tileDefs   []*tileDef
	doorDefs   []*doorDef
	objectDefs []*objectDef
	itemDefs   []*itemDef
	wordHashes = map[uint32]string{}
	rsaExp     *big.Int
	rsaMod     *big.Int
)

type client struct {
	*RSCPacket
	conn               net.Conn
	user               string
	pass               string
	packetChan         chan *RSCPacket
	lastSentPacketTime int64
	closed             chan struct{}
	closedLock         *sync.Mutex
	captchaBuf         []byte
	world              *world
	account            *account
	localWorld         *localWorld
	scriptKillSignal   chan struct{}
	scriptStopSignal   chan struct{}
	scriptDCSignal     chan int
	accounts           []*account
	progressTimer      *time.Timer
	progressTimerChan  <-chan time.Time
	scriptTimer        *time.Timer
	scriptTimerChan    <-chan time.Time
	botMessages        <-chan *botMessage

	script *script

	// Serve mode (see serve.go). calls is nil outside serve mode, so its
	// select case in eventLoop never fires there.
	calls         chan *call
	onLoaded      func()
	loginResponse int

	loading         bool
	serverIndex     int
	worldX          int
	worldZ          int
	regionX         int
	regionZ         int
	localX          int
	localZ          int
	planeIndex      int
	planeMultiplier int
	x               int
	z               int
	sprite          int

	currentStats              [skillCount]int
	baseStats                 [skillCount]int
	experience                [skillCount]int
	questPoints               int
	equipmentStats            [5]int
	inventory                 [30]*item
	inventoryCount            int
	players                   [500]*player
	playerCache               [500]*player
	playersServer             [4000]*player
	playerCacheCount          int
	playerCount               int
	npcs                      [500]*npc
	npcCache                  [500]*npc
	npcsServer                [5000]*npc
	npcCacheCount             int
	npcCount                  int
	objects                   [5000]*object
	objectCount               int
	wallObjects               [5000]*wallObject
	wallObjectCount           int
	groundItems               [5000]*groundItem
	groundItemCount           int
	prayers                   [50]bool
	fatigue                   int
	accurateFatigue           float64
	sleepingFatigue           int
	sleeping                  bool
	combatStyle               int
	optionMenuVisible         bool
	optionMenuCount           int
	optionMenu                [20]string
	banking                   bool
	bankItemCount             int
	maxBankItemCount          int
	bankItems                 []*bankItem
	tradeScreen1Active        bool
	tradeScreen2Active        bool
	tradeRecipient            int
	tradeAccepted             bool
	tradeRecipientAccepted    bool
	myTradeItems              [14]*tradeItem
	recipientTradeItems       [14]*tradeItem
	recipientTradeCount       int
	myTradeCount              int
	myTradeConfirmItems       [14]*tradeItem
	recipientConfirmItems     [14]*tradeItem
	myTradeConfirmItemCount   int
	recipientConfirmItemCount int
	tradeConfirmAccepted      bool
	shopping                  bool
	shopItemCount             int
	shopItems                 [256]*shopItem
	friendList                [200]*friend
	friendListCount           int
	ignoreList                [200]*ignored
	ignoreListCount           int
	questCount                int
	quests                    [100]*quest
	encodedChatBuffer         [4997]byte
	chatBuffer                [5096]byte
	decodedChatLength         int
	encodedChatLength         int
	appearanceChange          bool
	pathX                     [8000]int16
	pathZ                     [8000]int16
	pathFindSource            [96][96]int16
	systemUpdate              bool

	blackhole bool
}

type npc struct {
	id                 int
	serverIndex        int
	x                  int
	z                  int
	sprite             int
	currentHP          int
	maxHP              int
	waypointsX         [10]int16
	waypointsZ         [10]int16
	waypointCurrent    int8
	waypointNext       int8
	messageTime        time.Time
	lastMessageTimeout time.Duration
}

type player struct {
	serverIndex        int
	x                  int
	z                  int
	sprite             int
	currentHP          int
	maxHP              int
	combatLevel        int
	waypointsX         [10]int16
	waypointsZ         [10]int16
	waypointCurrent    int8
	waypointNext       int8
	username           string
	messageTime        time.Time
	lastMessageTimeout time.Duration
	skillTime          time.Time
	skillingTimeout    time.Duration
	clan               string
}

type object struct {
	id  int
	x   int
	z   int
	dir int
}

type wallObject struct {
	id  int
	x   int
	z   int
	dir int
}

type groundItem struct {
	id int
	x  int
	z  int
}

type bankItem struct {
	id     int
	amount int
}

type tradeItem struct {
	id     int
	amount int
}

type item struct {
	id       int
	amount   int
	equipped bool
	slot     int
}

type shopItem struct {
	id        int
	amount    int
	buyPrice  int
	sellPrice int
}

type friend struct {
	username string
	online   bool
}

type ignored struct {
	username string
	arg0     string
	arg1     string
	old      string
}

type quest struct {
	id    int
	name  string
	stage int
}

type tileDef struct {
	/*Colour     int
	TileValue  int*/
	ObjectType int
}

type doorDef struct {
	/*Name             string
	Description      string
	Command1         string
	Command2         string*/
	DoorType int
	Unknown  int
	/*WallObjectHeight int
	ModelVar2        int
	ModelVar3        int
	ID               int
	*/
}

type objectDef struct {
	/*Name          string
	Description   string
	Command1      string
	Command2      string*/
	Typ    int16
	Width  int16
	Height int16
	/*GroundItemVar int
	ObjectModel   string
	ID int*/
}

type itemDef struct {
	Name string
	/*ID int
	Description      string
	Command          string*/
	/*IsFemaleOnly     int
	IsMembersOnly    int*/
	IsStackable int
	/*IsUntradable     int
	IsWearable       int
	AppearanceID     int
	WearableID       int
	WearSlot         int
	RequiredLevel    int
	RequiredSkillID  int
	ArmourBonus      int
	WeaponAimBonus   int
	WeaponPowerBonus int
	MagicBonus       int
	PrayerBonus      int
	IsNoteable       int
	*/
	BasePrice int
}

type rsaKeyData struct {
	Exponent string
	Modulus  string
}

type bot struct {
	accounts  []*account
	wg        *sync.WaitGroup
	loginWait chan struct{}
	world     *world
	closing   chan struct{}
}

func main() {
	var (
		user          string
		pass          string
		scriptPath    string
		regFile       string
		configPath    string
		acctFile      string
		compilePath   string
		serveAddr     string
		progReport    int64
		progReportDur time.Duration
		autologin     bool
		downloadRSA   bool
		version       bool
		help          bool
		sigs          = make(chan os.Signal, 1)
	)

	b := &bot{
		accounts:  []*account{},
		wg:        &sync.WaitGroup{},
		loginWait: make(chan struct{}),
		closing:   make(chan struct{}),
		world:     newWorld(),
	}

	flag.StringVar(&user, "u", "", "OpenRSC username")
	flag.StringVar(&pass, "p", "", "OpenRSC password")
	flag.Int64Var(&progReport, "pr", int64(noProgressDurSet), "Progress report minutes")
	flag.StringVar(&regFile, "r", "", "Register accounts path")
	flag.StringVar(&scriptPath, "s", "", "Python script path")
	flag.StringVar(&acctFile, "f", "", "Account file")
	flag.StringVar(&configPath, "c", "", "Config file path")
	flag.StringVar(&compilePath, "compile", "", "Compile script at path")
	flag.StringVar(&serveAddr, "serve", "", "Serve the script API over HTTP on this loopback address, e.g. 127.0.0.1:8787 (no accounts are loaded from files)")
	flag.BoolVar(&downloadRSA, "d", false, "Download RSA keys")
	flag.BoolVar(&autologin, "l", false, "Autologin flag")
	flag.BoolVar(&version, "v", false, "Version flag")
	flag.BoolVar(&help, "h", false, "Help")
	flag.Parse()

	if version {
		fmt.Printf("Plutonium v%s\n", botVersion)
		return
	}

	if help {
		fmt.Printf("Plutonium v%s command line arguments:\n\n", botVersion)
		flag.PrintDefaults()
		return
	}

	parseConfig(configPath)

	if downloadRSA {
		downloadRSAKeys()
		return
	}

	if regFile != "" {
		doAccountCreations(regFile)
		return
	}

	if compilePath != "" {
		compileScript(compilePath)
		return
	}

	progReportDur = time.Duration(progReport)
	if progReportDur != noProgressDurSet {
		if progReportDur <= 0 {
			fmt.Printf("Progress report field should be > 0")
			os.Exit(1)
		}
		progReportDur *= time.Minute
	}

	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	if serveAddr != "" {
		b.checkOCR()
		loadDefs()
		b.world.loadWorld()
		b.loginWaiter()
		runServer(b, serveAddr, sigs)
		return
	}

	go func() {
		<-sigs
		fmt.Println("[BOT] Received kill signal")
		close(b.closing)
	}()

	b.checkOCR()
	loadDefs()
	b.world.loadWorld()
	b.loadAccounts(user, pass, acctFile, scriptPath, autologin, progReportDur)
	b.loginWaiter()
	b.runAccounts()

	fmt.Println("[BOT] All accounts are finished, exiting")
}

func (b *bot) loadAccounts(user, pass, acctFile, scriptPath string, autologin bool, progReportDur time.Duration) {
	var err error
	if user == "" && pass == "" {
		if acctFile == "" {
			b.accounts, err = loadAccounts()
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
		} else {
			acct, err := loadAccount(acctFile)
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			b.accounts = append(b.accounts, acct)
		}
	} else {
		acct := &account{
			username:        user,
			password:        pass,
			autologin:       autologin,
			enabled:         true,
			debug:           true,
			progressDur:     progReportDur,
			needsFirstNotif: true,
			botMessages:     make(chan *botMessage, botMessagesSize),
			botMessageLock:  &sync.Mutex{},
		}
		if scriptPath != "" {
			pathDir := filepath.Dir(scriptPath)
			acct.script = &script{
				path:    scriptPath,
				pathDir: pathDir,
			}
		}
		b.accounts = append(b.accounts, acct)
	}

	if len(b.accounts) == 0 {
		fmt.Println("[BOT] You need account files to run Plutonium")
		os.Exit(1)
	}
}

func (b *bot) checkOCR() {
	if settings.OcrSettings.OcrType == "local" {
		_, err := os.Stat(filepath.Join(settings.executablePath, settings.OcrSettings.OcrFileName))
		if os.IsNotExist(err) {
			fmt.Printf("[BOT] OCR not found: %s\n", err)
			os.Exit(1)
		} else if err != nil {
			fmt.Printf("[BOT] OCR Error: %s\n", err)
			os.Exit(1)
		}
	}
}

func (b *bot) loginWaiter() {
	if len(b.accounts) > 1 {
		fmt.Println("[BOT] NOTE: Plutonium has to stagger logins by 6 seconds so as to not spam the server.")
	}
	go func() {
		for {
			<-b.loginWait
			time.Sleep(loginStagger)
		}
	}()
}

func (b *bot) runAccounts() {
	for _, acc := range b.accounts {
		if !acc.enabled {
			continue
		}
		f := acc.mustCreateProgressFile()
		defer f.Close()

		b.wg.Add(1)
		go b.runClientLoop(acc)
	}
	b.wg.Wait()
}

func (acc *account) mustCreateProgressFile() *os.File {
	var err error
	acc.progressFile, err = os.Create(filepath.Join(settings.executablePath, settings.LogSettings.Directory, settings.LogSettings.ProgressReports, acc.username+".log"))
	if err != nil {
		fmt.Printf("[%s] Error creating progress report file: %s\n", acc.username, err)
		os.Exit(1)
	}
	acc.lastProgressReport = time.Now()

	return acc.progressFile
}

func (b *bot) runClientLoop(acc *account) {
clientLoop:
	for {
		c := &client{
			RSCPacket:        &RSCPacket{buf: make([]byte, 5000)},
			user:             acc.username,
			pass:             acc.password,
			world:            b.world,
			closed:           make(chan struct{}),
			closedLock:       &sync.Mutex{},
			packetChan:       make(chan *RSCPacket, 32),
			loading:          true,
			account:          acc,
			localWorld:       newLocalWorld(),
			scriptKillSignal: make(chan struct{}, 1),
			scriptStopSignal: make(chan struct{}, 1),
			scriptDCSignal:   make(chan int, 1),
			script:           acc.script,
			accounts:         b.accounts,
		}

		if c.script != nil {
			err := c.loadScript()
			c.script.dumpError(nil, err)

			if c.account.needsFirstNotif {
				c.account.writeFirstNotif(c.script.onProgressReport == nil)
			}
		}

		select {
		case b.loginWait <- struct{}{}:
		case <-b.closing:
			break clientLoop
		}

		success, loginWait, err := c.login()
		if err != nil {
			fmt.Printf("[%s] Login error: %s\n", c.user, err)
		}

		if !success {
			select {
			case <-time.After(loginWait):
				continue clientLoop
			case <-b.closing:
				break clientLoop
			}
		}

		c.init()

		go c.readHandler()

		shouldBreak, reloginWait := b.eventLoop(c)
		if shouldBreak || !acc.autologin {
			break
		}

		if reloginWait == 0 {
			reloginWait = 5 * time.Second
		}

		select {
		case <-time.After(reloginWait):
			continue clientLoop
		case <-b.closing:
			break clientLoop
		}
	}
	b.wg.Done()
}

func (c *client) readHandler() {
out:
	for {
		select {
		case <-c.closed:
			break out
		default:
			message, err := c.readPacket()
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
					fmt.Printf("[%s] Read error: %s\n", c.user, err)
				}
				c.close()
				break out
			}

			select {
			case <-c.closed:
				break out
			case c.packetChan <- message:
			}
		}
	}
	c.conn.Close()
}

func (c *client) initChans() {
	if c.script != nil && c.script.running {
		c.scriptTimer = time.NewTimer(0)
		c.scriptTimerChan = c.scriptTimer.C
		if c.account.progressDur != noProgressDurSet {
			c.progressTimer = time.NewTimer(c.account.progressDur - time.Since(c.account.lastProgressReport))
			c.progressTimerChan = c.progressTimer.C
		}
		c.botMessages = c.account.botMessages
	}
}

func (b *bot) eventLoop(c *client) (bool, time.Duration) {
	var (
		idleTimer     = time.NewTimer(5 * time.Second)
		localClosing  = b.closing
		scriptStopped = false
	)

	defer func() {
		if c.progressTimer != nil && !c.progressTimer.Stop() {
			<-c.progressTimer.C
		}
		if c.scriptTimer != nil && !scriptStopped && !c.scriptTimer.Stop() {
			<-c.scriptTimer.C
		}
		if !idleTimer.Stop() {
			<-idleTimer.C
		}
	}()

loop:
	for {
		select {
		case <-c.closed:
			break loop
		case <-idleTimer.C:
			diff := time.Now().Unix() - c.lastSentPacketTime

			if diff >= 5 {
				c.createPacket(67)
				c.sendPacket()

				idleTimer.Reset(5 * time.Second)
			} else {
				idleTimer.Reset(time.Duration(5-diff) * time.Second)
			}
		case packet := <-c.packetChan:
			opcode := packet.readByteAsInt()
			c.handlePacket(packet, opcode, len(packet.buf)-1)
		case <-c.progressTimerChan:
			c.account.lastProgressReport = time.Now()
			if c.script != nil && c.script.running && c.script.onProgressReport != nil {
				c.script.callOnProgressReport()
				c.account.writeProgressReportf("Next progress report scheduled for %s", time.Now().Add(c.account.progressDur).Format(time.RFC1123))
			}
			c.progressTimer.Reset(c.account.progressDur)
		case <-c.scriptKillSignal:
			c.close()
			fmt.Printf("[%s] Script kill signal received, terminating\n", c.user)
			return true, 0
		case <-c.scriptStopSignal:
			if !c.scriptTimer.Stop() {
				<-c.scriptTimer.C
			}
			c.script.running = false
			scriptStopped = true
		case waitTime := <-c.scriptDCSignal:
			fmt.Printf("[%s] Disconnecting for %d seconds\n", c.user, waitTime)
			c.conn.Close()
			c.close()
			return false, time.Second * time.Duration(waitTime)
		case <-c.scriptTimerChan:
			if !c.sleeping {
				if !c.script.inited {
					if c.script.onInit != nil {
						c.script.callOnInit()
					}
					c.script.inited = true
				}
				c.scriptTimer.Reset(time.Duration(c.script.loop()) * time.Millisecond)
			} else {
				c.scriptTimer.Reset(50 * time.Millisecond)
			}
		case cl := <-c.calls:
			cl.invoke(c)
		case msg := <-c.botMessages:
			if c.script != nil && c.script.onBotMessage != nil {
				c.script.callOnBotMessage(msg.sender, msg.dict.Copy())
			}
		case <-localClosing:
			if c.script != nil && c.script.running {
				if c.script.onKillSignal != nil {
					fmt.Printf("[%s] Waiting for script to clean up before terminating\n", c.user)
					c.script.callOnKillSignal()
					localClosing = nil
					go func() {
						time.Sleep(15 * time.Second)
						c.close()
					}()
				} else {
					c.close()
					fmt.Printf("[%s] No kill signal handler, terminating\n", c.user)
					return true, 0
				}
			} else {
				c.close()
				fmt.Printf("[%s] Script not running, terminating\n", c.user)
				return true, 0
			}
		}
	}

	return false, 0
}

func (c *client) close() {
	c.closedLock.Lock()
	defer c.closedLock.Unlock()

	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
}

func loadDefs() {
	fmt.Printf("[BOT] Loading definitions...")
	dataDir := filepath.Join(settings.executablePath, settings.DataSettings.Directory)
	// tile defs
	f1, err := os.Open(filepath.Join(dataDir, settings.DataSettings.TileDefs))
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer f1.Close()

	err = json.NewDecoder(f1).Decode(&tileDefs)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// door defs
	f2, err := os.Open(filepath.Join(dataDir, settings.DataSettings.DoorDefs))
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer f2.Close()

	err = json.NewDecoder(f2).Decode(&doorDefs)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// object defs
	f3, err := os.Open(filepath.Join(dataDir, settings.DataSettings.ObjectDefs))
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer f3.Close()

	err = json.NewDecoder(f3).Decode(&objectDefs)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// item defs
	f4, err := os.Open(filepath.Join(dataDir, settings.DataSettings.ItemDefs))
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer f4.Close()

	err = json.NewDecoder(f4).Decode(&itemDefs)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// word hashes
	f5, err := os.Open(filepath.Join(dataDir, settings.DataSettings.WordHashes))
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer f5.Close()

	err = json.NewDecoder(f5).Decode(&wordHashes)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// rsa keys
	f6, err := os.Open(filepath.Join(dataDir, settings.DataSettings.RSAKeyFile))
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer f6.Close()

	rsaKeys := &rsaKeyData{}
	err = json.NewDecoder(f6).Decode(rsaKeys)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	var ok bool
	if rsaExp, ok = new(big.Int).SetString(rsaKeys.Exponent, 10); !ok {
		fmt.Println("Error decoding RSA exponent")
		os.Exit(1)
	}
	if rsaMod, ok = new(big.Int).SetString(rsaKeys.Modulus, 10); !ok {
		fmt.Println("Error decoding RSA modulus")
		os.Exit(1)
	}

	fmt.Println("complete")
}

func (acc *account) writeFirstNotif(noProgressReport bool) {
	if acc.progressDur == noProgressDurSet {
		acc.writeProgressReportf("You have not configured progress reports")
	} else if noProgressReport {
		acc.writeProgressReportf("Your script is not designed to output progress reports")
	} else {
		tdiff := acc.progressDur - time.Since(acc.lastProgressReport)
		acc.writeProgressReportf("Next progress report scheduled for %s", time.Now().Add(tdiff).Format(time.RFC1123))
	}
	acc.needsFirstNotif = false
}

func downloadRSAKeys() {
	conn, err := net.DialTimeout("tcp4", settings.ServerSettings.Address, connectTimeout*time.Second)
	if err != nil {
		fmt.Printf("Error connecting to server: %s\n", err)
		return
	}

	defer conn.Close()

	sessionPacket, err := readBytesRaw(conn, 4)
	if err != nil {
		fmt.Printf("Error reading session packet: %s\n", err)
		return
	}
	_ = sessionPacket

	p := &RSCPacket{buf: make([]byte, 3)}

	p.createPacket(19)
	p.finish()

	if err = writePacket(conn, p); err != nil {
		fmt.Printf("Error writing packet: %s\n", err)
		return
	}

	lengthBuf, err := readBytesRaw(conn, 2)
	if err != nil {
		fmt.Printf("Error reading packet length: %s\n", err)
		return
	}

	length := ((int(lengthBuf[0]) << 8) | int(lengthBuf[1])) - 2
	if length < 0 || length > maxPacketSize {
		fmt.Printf("Invalid length from server: %d\n", length)
		return
	}

	bs, err := readBytesRaw(conn, length)
	if err != nil {
		fmt.Printf("Error reading config: %s\n", err)
		return
	}
	p = &RSCPacket{buf: bs}

	p.readString()
	p.readString()
	p.i += 39
	p.readString()
	p.i += 2
	p.readString()
	p.i += 41

	exponent := p.readString()
	modulus := p.readString()

	keys := &rsaKeyData{
		Exponent: exponent,
		Modulus:  modulus,
	}

	path := filepath.Join(settings.executablePath, settings.DataSettings.Directory, settings.DataSettings.RSAKeyFile)

	f, err := os.Create(path)
	if err != nil {
		fmt.Printf("Error opening RSA key file: %s\n", err)
		return
	}
	defer f.Close()

	err = json.NewEncoder(f).Encode(keys)
	if err != nil {
		fmt.Printf("Error writing RSA key file: %s\n", err)
		return
	}

	fmt.Printf("Saved RSA keys to %s\n", path)
}

func doAccountCreations(fileStr string) {
	f, err := os.Open(fileStr)
	if err != nil {
		fmt.Printf("Error opening register account file: %s\n", err)
		os.Exit(1)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var (
		username string
		password string
		email    string
	)
	for scanner.Scan() {
		time.Sleep(2 * time.Second)

		line := scanner.Text()
		if line == "" {
			break
		}
		lineSplit := strings.Split(line, ":")
		username = lineSplit[0]
		password = lineSplit[1]
		email = lineSplit[2]
	retry:
		registerResponse, err := registerAccount(username, password, email)
		if err != nil {
			fmt.Printf("[%s] %s\n", username, err)
			if errors.Is(err, errRegisterConnect) {
				return
			}
			continue
		}
		switch registerResponse {
		case 255:
			fmt.Printf("[%s] Registration failed\n", username)
		case 0:
			fmt.Printf("[%s] Account created\n", username)
		case 2:
			fmt.Printf("[%s] Username already taken\n", username)
		case 3:
			fmt.Printf("[%s] Email already in use\n", username)
		case 4:
			fmt.Printf("[%s] Registration disabled\n", username)
			return
		case 5:
			fmt.Printf("[%s] You have registered too recently, waiting 5 minutes\n", username)
			time.Sleep(5 * time.Minute)
			goto retry
		case 6:
			fmt.Printf("[%s] Invalid email address\n", username)
		case 7:
			fmt.Printf("[%s] Username must be 2-12 characters long\n", username)
		case 8:
			fmt.Printf("[%s] Invalid username\n", username)
		default:
			fmt.Printf("[%s] Unknown registration response: %d\n", username, registerResponse)
		}
	}
}

var errRegisterConnect = errors.New("could not connect for account creation")

// registerAccount performs one registration exchange with the game server
// and returns its response code (0 = created). Shared by -r and serve mode.
func registerAccount(username, password, email string) (byte, error) {
	conn, err := net.DialTimeout("tcp4", settings.ServerSettings.Address, connectTimeout*time.Second)
	if err != nil {
		return 0, fmt.Errorf("%w: %s", errRegisterConnect, err)
	}
	defer conn.Close()

	if _, err := readBytesRaw(conn, 4); err != nil {
		return 0, fmt.Errorf("%w: reading session packet: %s", errRegisterConnect, err)
	}

	p := &RSCPacket{buf: make([]byte, 5000)}
	p.createPacket(2)
	p.writeBytes([]byte(username))
	p.writeByte(10)
	p.writeBytes(encodeCredential(password))
	p.writeByte(10)
	p.writeBytes([]byte(email))
	p.writeByte(10)
	p.finish()

	if err := writePacket(conn, p); err != nil {
		return 0, fmt.Errorf("writing registration packet: %s", err)
	}

	resp, err := readBytesRaw(conn, 1)
	if err != nil {
		return 0, fmt.Errorf("reading registration response: %s", err)
	}
	return resp[0], nil
}

func writePacket(conn net.Conn, p *RSCPacket) error {
	wrote := 0
	for wrote < p.i {
		conn.SetWriteDeadline(time.Now().Add(time.Second * readWriteTimeout))
		n, err := conn.Write(p.buf[wrote:p.i])
		if err != nil {
			return err
		}
		wrote += n
	}

	p.i = 0

	return nil
}

func (a *account) writeProgressReportf(s0 string, args ...any) error {
	_, err := a.progressFile.Write([]byte(fmt.Sprintf(s0, args...)))
	if err != nil {
		fmt.Printf("[%s] Unable to save progress report: %s\n", a.username, err)
		return err
	}
	if runtime.GOOS == "windows" {
		_, err = a.progressFile.Write([]byte("\r\n"))
	} else {
		_, err = a.progressFile.Write([]byte{'\n'})
	}
	return err
}

func (c *client) init() {
	for i := 0; i < len(c.inventory); i++ {
		c.inventory[i] = &item{}
	}
	for i := 0; i < len(c.groundItems); i++ {
		c.groundItems[i] = &groundItem{}
	}
	for i := 0; i < len(c.objects); i++ {
		c.objects[i] = &object{}
	}
	for i := 0; i < len(c.wallObjects); i++ {
		c.wallObjects[i] = &wallObject{}
	}
	for i := 0; i < len(c.ignoreList); i++ {
		c.ignoreList[i] = &ignored{}
	}
	for i := 0; i < len(c.myTradeItems); i++ {
		c.myTradeItems[i] = &tradeItem{}
	}
}

func (c *client) doCaptcha(buf []byte) {
	var sleepWord string

	switch settings.OcrSettings.OcrType {
	case "local":
		bmpData := convertImage(buf)
		if bmpData == nil {
			return
		}
		num := rand.Intn(math.MaxInt)
		randName := hex.EncodeToString([]byte{byte(num >> 24), byte(num >> 16), byte(num >> 8), byte(num)}) + ".bmp"
		path := filepath.Join(settings.executablePath, settings.OcrSettings.Directory, randName)
		f, err := os.Create(path)
		if err != nil {
			fmt.Printf("[%s] Error creating sleep bmp file: %s\n", c.user, err)
			return
		}
		_, err = io.Copy(f, bytes.NewReader(bmpData))
		f.Close()
		if err != nil {
			fmt.Printf("[%s] Error copying sleepword bytes to file: %s\n", c.user, err)
			os.Remove(path)
			return
		}
		cmd := exec.Command(filepath.Join(settings.executablePath, settings.OcrSettings.OcrFileName), path)
		bs, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("[%s] Error getting sleepword from external ocr: %s\n", c.user, err)
			os.Remove(path)
			return
		}
		sleepWord = strings.Trim(string(bs), " \r\n\t")
		os.Remove(path)
	case "hash":
		hasher := fnv.New32()
		wrote := 0
		for wrote < len(buf) {
			n, _ := hasher.Write(buf[wrote:])
			wrote += n
		}
		hash := hasher.Sum32()

		var ok bool
		if sleepWord, ok = wordHashes[hash]; !ok {
			sleepWord = "unknown"
		}
	default:
		fmt.Printf("[%s] Unknown ocr setting\n", c.user)
		return
	}

	c.sendSleepWord(sleepWord)
	if c.script != nil && c.script.onSleepWordSent != nil {
		c.script.callOnSleepWordSent()
	}
}

func readBytesRaw(conn net.Conn, length int) ([]byte, error) {
	buf := make([]byte, length)
	readLen := 0
	for readLen < length {
		conn.SetReadDeadline(time.Now().Add(time.Second * readWriteTimeout))
		n, err := conn.Read(buf[readLen:])
		if err != nil {
			return nil, err
		}
		readLen += n
	}
	return buf, nil
}

func encodeCredential(s string) []byte {
	s1 := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			s1[i] = c
		} else if c >= 'A' && c <= 'Z' {
			s1[i] = c
		} else if c >= '0' && c <= '9' {
			s1[i] = c
		} else {
			s1[i] = '_'
		}
	}

	return s1
}

func encodePassword(s string) []byte {
	s1 := make([]byte, 20)
	for i := 0; i < 20; i++ {
		if i >= len(s) {
			s1[i] = ' '
			continue
		}
		c := s[i]
		if c >= 'a' && c <= 'z' {
			s1[i] = c
		} else if c >= 'A' && c <= 'Z' {
			s1[i] = c
		} else if c >= '0' && c <= '9' {
			s1[i] = c
		} else {
			s1[i] = '_'
		}
	}

	return s1
}

func encryptRSA(bs []byte) []byte {
	bs = append(bs, 10) // this is here to keep parity with the official openrsc client

	base := new(big.Int).SetBytes(bs)

	result := new(big.Int)
	result.Exp(base, rsaExp, rsaMod)

	bs = result.Bytes()
	// java expects a 0 prepended if the leading bit is 1
	if bs[0]&0x80 != 0 {
		bs = append([]byte{0}, bs...)
	}
	return bs
}

func (c *client) login() (bool, time.Duration, error) {
	c.loginResponse = -1
	conn, err := net.DialTimeout("tcp4", settings.ServerSettings.Address, connectTimeout*time.Second)
	if err != nil {
		return false, time.Second * 15, fmt.Errorf("connection error: %s", err)
	}
	c.conn = conn

	sessionPacket, err := readBytesRaw(conn, 4)
	if err != nil {
		return false, time.Second * 15, fmt.Errorf("read session id error: %s", err)
	}
	_ = sessionPacket

	encryptedPass := encryptRSA(encodePassword(c.pass))
	loginDetails := encryptRSA([]byte("plutonium"))

	c.createPacket(0)
	c.writeByte(0)
	c.writeInt(settings.ClientSettings.ClientVersion)
	c.writeBytes(encodeCredential(c.user))
	c.writeByte(10)
	c.writeByte(1) // 0 = plain, 1 = rsa
	c.writeShort(len(encryptedPass))
	c.writeBytes(encryptedPass)
	c.writeShort(len(loginDetails))
	c.writeBytes(loginDetails)
	c.writeLong(0)

	c.writeShort(228)
	c.writeInt(1289)
	c.writeInt(793)
	c.writeInt(1188)
	c.writeShort(13)
	c.writeShort(47)
	c.writeByte(17)
	c.writeShort(5)
	c.writeShort(54)
	c.writeShort(24)
	c.writeInt(213)
	c.writeByte(1)
	c.writeShort(6)
	c.writeInt(4)
	c.writeInt(9)
	c.writeInt(14)
	c.writeShort(49)

	c.writeInt(37) // sound cache
	c.writeByte(0) // crown count
	c.writeByte(5)
	c.writeInt(48 * 4)
	c.writeBytes([]byte("63"))
	c.writeByte(10)

	c.sendPacket()

	var loginResponse byte
	loginRespBuf, err := readBytesRaw(conn, 1)
	if err != nil {
		return false, time.Second * 15, err
	}
	loginResponse = loginRespBuf[0]
	c.loginResponse = int(loginResponse)

	fmt.Printf("[%s] Login response: %d\n", c.user, loginResponse)

	if loginResponse&0x40 == 0 {
		switch loginResponse {
		case 4: // currently logged in
			fmt.Printf("[%s] Account already logged in\n", c.user)
			return false, time.Second * 5, nil
		case 100: // something
			fmt.Printf("[%s] Weird server response\n", c.user)
			return false, time.Second * 5, nil
		case 3:
			fmt.Printf("[%s] Invalid username or password\n", c.user)
			return false, time.Second * 5, nil
		case 5:
			fmt.Printf("[%s] The client has been updated\n", c.user)
			return false, time.Second * 5, nil
		case 6:
			fmt.Printf("[%s] You may only use 1 character at once, waiting 5-7 minutes\n", c.user)
			return false, rand5To7Minutes(), nil
		case 7:
			fmt.Printf("[%s] Login attempts exceeded, waiting 5-7 minutes\n", c.user)
			return false, rand5To7Minutes(), nil
		case 8:
			fmt.Printf("[%s] Server rejected session\n", c.user)
			return false, time.Second * 5, nil
		case 9:
			fmt.Printf("[%s] Error unable to login\n", c.user)
			return false, time.Second * 5, nil
		case 10:
			fmt.Printf("[%s] Username already in use\n", c.user)
			return false, time.Second * 10, nil
		case 11:
			fmt.Printf("[%s] Account temporarily disabled, waiting 5-7 minutes\n", c.user)
			return false, rand5To7Minutes(), nil
		case 12:
			fmt.Printf("[%s] Account permanently disabled, waiting 5-7 minutes\n", c.user)
			return false, rand5To7Minutes(), nil
		case 14:
			fmt.Printf("[%s] World full\n", c.user)
			return false, time.Second * 5, nil
		case 15, 16, 17, 18, 20, 21, 22, 23, 24, 25:
			fmt.Printf("[%s] Generic server error, waiting 5-7 minutes\n", c.user)
			return false, rand5To7Minutes(), nil
		default:
			fmt.Printf("[%s] Unhandled login response \n", c.user)
			return false, time.Minute, nil
		}
	}

	return true, 0, nil
}

func rand5To7Minutes() time.Duration {
	return (5 * time.Minute) + time.Duration(rand.Int63n(int64(2*time.Minute)))
}

func (c *client) sendPacket() error {
	c.finish()
	wrote := 0
	length := c.RSCPacket.i
	var (
		err error
		n   int
	)
	for wrote < length {
		c.conn.SetWriteDeadline(time.Now().Add(time.Second * readWriteTimeout))
		n, err = c.conn.Write(c.RSCPacket.buf[wrote:length])
		if err != nil {
			c.close()
			fmt.Printf("[%s] Error writing packet to stream: %s\n", c.user, err)
			break
		}
		wrote += n
	}
	c.RSCPacket.i = 0
	c.lastSentPacketTime = time.Now().Unix()
	return err
}

func (c *client) readPacket() (*RSCPacket, error) {
	lenPacketRead := 0
	lenBuf := make([]byte, 2)
	for lenPacketRead < 2 {
		c.conn.SetReadDeadline(time.Now().Add(time.Second * readWriteTimeout))
		n, err := c.conn.Read(lenBuf[lenPacketRead:])
		if err != nil {
			return nil, err
		}
		lenPacketRead += n
	}
	length := ((int(lenBuf[0]) << 8) | int(lenBuf[1])) - 2

	if length > maxPacketSize {
		return nil, fmt.Errorf("packet size too large: %d", length)
	}

	readLength := 0

	buf := make([]byte, length)
	for readLength < length {
		c.conn.SetReadDeadline(time.Now().Add(time.Second * readWriteTimeout))
		n, err := c.conn.Read(buf[readLength:])
		if err != nil {
			return nil, err
		}
		readLength += n
	}
	return &RSCPacket{buf: buf}, nil
}

func (c *client) createPlayer(serverIndex, x, z, sprite int) {
	var p *player
	if p = c.playersServer[serverIndex]; p == nil {
		p = &player{serverIndex: serverIndex}
		c.playersServer[serverIndex] = p
	}

	found := false
	for i := 0; i < c.playerCacheCount; i++ {
		if c.playerCache[i].serverIndex == serverIndex {
			found = true
			break
		}
	}

	p.sprite = sprite
	p.x = x
	p.z = z

	if found {
		if int16(x) != p.waypointsX[p.waypointCurrent] || int16(z) != p.waypointsZ[p.waypointCurrent] {
			p.waypointCurrent = (p.waypointCurrent + 1) % 10
			p.waypointsX[p.waypointCurrent] = int16(x)
			p.waypointsZ[p.waypointCurrent] = int16(z)
		}
	} else {
		p.serverIndex = serverIndex
		p.waypointCurrent = 0
		p.waypointNext = 0
		p.waypointsX[p.waypointCurrent] = int16(x)
		p.waypointsZ[p.waypointCurrent] = int16(z)
	}
	c.players[c.playerCount] = p
	c.playerCount++
}

func (c *client) createNPC(serverIndex, id, x, z, sprite int) *npc {
	var n *npc
	if n = c.npcsServer[serverIndex]; n == nil {
		n = &npc{serverIndex: serverIndex}
		c.npcsServer[serverIndex] = n
	}

	found := false
	for i := 0; i < c.npcCacheCount; i++ {
		if c.npcCache[i].serverIndex == serverIndex {
			found = true
			break
		}
	}

	n.sprite = sprite
	n.id = id
	n.x = x
	n.z = z

	if found {
		if int16(x) != n.waypointsX[n.waypointCurrent] || int16(z) != n.waypointsZ[n.waypointCurrent] {
			n.waypointCurrent = (n.waypointCurrent + 1) % 10
			n.waypointsX[n.waypointCurrent] = int16(x)
			n.waypointsZ[n.waypointCurrent] = int16(z)
		}
	} else {
		n.serverIndex = serverIndex
		n.waypointCurrent = 0
		n.waypointNext = 0
		n.waypointsX[n.waypointCurrent] = int16(x)
		n.waypointsZ[n.waypointCurrent] = int16(z)
		if c.script != nil && c.script.onNPCSpawned != nil {
			c.script.callOnNPCSpawned(n)
		}
	}
	c.npcs[c.npcCount] = n
	c.npcCount++

	if found {
		return n
	}

	return nil
}

func calculatePrice(basePrice, shopItemPrice, shopBuyPriceMod, shopPriceMultiplier, count, var5 int, var4 bool) int {
	cost := 0

	for k := 0; var5 > k; k++ {
		k0 := k
		if var4 {
			k0 = -k
		}
		var10 := shopPriceMultiplier * (shopItemPrice + (k0 - count))
		if var10 >= -100 {
			if var10 > 100 {
				var10 = 100
			}
		} else {
			var10 = -100
		}
		scaling := shopBuyPriceMod + var10
		if scaling < 10 {
			scaling = 10
		}

		cost += basePrice * scaling / 100
	}

	return cost
}

func (c *client) handlePacket(p *RSCPacket, opcode, length int) {
	switch opcode {
	case 191: // Player coords
		c.playerCacheCount = c.playerCount
		for i := 0; i < c.playerCacheCount; i++ {
			c.playerCache[i] = c.players[i]
		}
		buf := p.readBytes(length)
		ofs := 0
		x := readBits(buf, ofs, 11)
		ofs += 11
		z := readBits(buf, ofs, 13)
		ofs += 13
		sprite := readBits(buf, ofs, 4)
		ofs += 4

		count := readBits(buf, ofs, 8)
		ofs += 8

		c.playerCount = 0

		c.createPlayer(c.serverIndex, x, z, sprite)

		c.x = x
		c.z = z
		wantX := x + c.worldX
		wantZ := z + c.worldZ
		regionX := (wantX + 24) / 48
		regionZ := (wantZ + 24) / 48

		c.regionX = (regionX*48 - 48) - c.worldX
		c.regionZ = (regionZ*48 - 48) - c.worldZ
		c.localX = c.x - c.regionX
		c.localZ = c.z - c.regionZ

		c.sprite = sprite

		for i := 0; count > i; i++ {
			player := c.playerCache[i+1]

			needsUpdate := readBits(buf, ofs, 1)
			ofs++
			if needsUpdate != 0 {
				updateType := readBits(buf, ofs, 1)
				ofs++
				if updateType != 0 {
					needsNextSprite := readBits(buf, ofs, 2)
					ofs += 2
					if needsNextSprite == 3 { // removed player / in combat
						continue
					} else {
						player.sprite = readBits(buf, ofs, 2) + (needsNextSprite << 2)
						ofs += 2
					}
				} else {
					dir := readBits(buf, ofs, 3)
					waypointX := player.waypointsX[player.waypointCurrent]
					waypointZ := player.waypointsZ[player.waypointCurrent]
					if dir == 2 || dir == 1 || dir == 3 {
						waypointX++
					}
					if dir == 6 || dir == 5 || dir == 7 {
						waypointX--
					}
					if dir == 4 || dir == 3 || dir == 5 {
						waypointZ++
					}
					if dir == 0 || dir == 1 || dir == 7 {
						waypointZ--
					}
					player.waypointCurrent = (player.waypointCurrent + 1) % 10
					player.waypointsX[player.waypointCurrent] = waypointX
					player.waypointsZ[player.waypointCurrent] = waypointZ
					player.x = int(waypointX)
					player.z = int(waypointZ)
					player.sprite = dir
					ofs += 3
				}
			}

			c.players[c.playerCount] = player
			c.playerCount++
		}

		for ofs+24 < length*8 {
			serverIndex := readBits(buf, ofs, 11)
			ofs += 11
			offsetX := readBits(buf, ofs, 6)
			ofs += 6
			offsetZ := readBits(buf, ofs, 6)
			ofs += 6
			sprite := readBits(buf, ofs, 4)
			ofs += 4

			if offsetX > 31 {
				offsetX -= 64
			}

			if offsetZ > 31 {
				offsetZ -= 64
			}

			c.createPlayer(serverIndex, c.x+offsetX, c.z+offsetZ, sprite)
		}
	case 79: // npc coords
		buf := p.readBytes(length)
		ofs := 0
		count := readBits(buf, ofs, 8)
		ofs += 8

		c.npcCacheCount = c.npcCount

		c.npcCount = 0

		for i := 0; i < c.npcCacheCount; i++ {
			c.npcCache[i] = c.npcs[i]
		}

		var removedNPCs map[*npc]struct{}
		if c.script != nil && c.script.onNPCDespawned != nil {
			removedNPCs = make(map[*npc]struct{})
		}

		for i := 0; i < count; i++ {
			npc := c.npcCache[i]

			var7 := readBits(buf, ofs, 1)
			ofs++
			if var7 != 0 {
				var12 := readBits(buf, ofs, 1)
				ofs++
				if var12 != 0 {
					nextSpriteOffset := readBits(buf, ofs, 2)
					ofs += 2
					if nextSpriteOffset == 3 { // npc remove / incombat
						if removedNPCs != nil {
							removedNPCs[npc] = struct{}{}
						}
						continue
					}
					npc.sprite = (nextSpriteOffset << 2) + readBits(buf, ofs, 2)
					ofs += 2
				} else { // npc walk
					dir := readBits(buf, ofs, 3)
					waypointX := npc.waypointsX[npc.waypointCurrent]
					waypointZ := npc.waypointsZ[npc.waypointCurrent]
					if dir == 2 || dir == 1 || dir == 3 {
						waypointX++
					}
					if dir == 6 || dir == 5 || dir == 7 {
						waypointX--
					}
					if dir == 4 || dir == 3 || dir == 5 {
						waypointZ++
					}
					if dir == 0 || dir == 1 || dir == 7 {
						waypointZ--
					}
					npc.waypointCurrent = (npc.waypointCurrent + 1) % 10
					npc.waypointsX[npc.waypointCurrent] = waypointX
					npc.waypointsZ[npc.waypointCurrent] = waypointZ
					npc.x = int(waypointX)
					npc.z = int(waypointZ)
					npc.sprite = dir
					ofs += 3
				}
			}

			c.npcs[c.npcCount] = npc
			c.npcCount++
		}

		for ofs+34 < length*8 {
			serverIndex := readBits(buf, ofs, 12)
			ofs += 12
			x := readBits(buf, ofs, 6)
			ofs += 6
			z := readBits(buf, ofs, 6)
			ofs += 6
			sprite := readBits(buf, ofs, 4)
			ofs += 4

			if x > 31 {
				x -= 64
			}

			if z > 31 {
				z -= 64
			}

			id := readBits(buf, ofs, 10)
			ofs += 10

			if n := c.createNPC(serverIndex, id, c.x+x, c.z+z, sprite); n != nil {
				if removedNPCs != nil {
					delete(removedNPCs, n)
				}
			}
		}

		for k := range removedNPCs {
			c.script.callOnNPCDespawned(k)
		}

		if c.script != nil && c.script.onServerTick != nil {
			c.script.callOnServerTick()
		}
	case 25: // load area
		c.serverIndex = p.readShort()
		c.worldX = p.readShort()
		c.worldZ = p.readShort()
		c.planeIndex = p.readShort()
		c.planeMultiplier = p.readShort()
		if c.account.debug {
			fmt.Printf("[%s] Server Index: %d\n", c.user, c.serverIndex)
		}
	case 211: // remove world entities
		count := length / 4

		for i := 0; i < count; i++ {
			x1 := (c.x + int(int16(p.readShort()))) >> 3
			z1 := (c.z + int(int16(p.readShort()))) >> 3

			groundItemCount := 0
			for j := 0; j < c.groundItemCount; j++ {
				x2 := (c.groundItems[j].x >> 3) - x1
				z2 := (c.groundItems[j].z >> 3) - z1
				if x2 != 0 || z2 != 0 {
					if j != groundItemCount {
						c.groundItems[groundItemCount].id = c.groundItems[j].id
						c.groundItems[groundItemCount].x = c.groundItems[j].x
						c.groundItems[groundItemCount].z = c.groundItems[j].z
					}
					groundItemCount++
				} else {
					if c.script != nil && c.script.onGroundItemDespawned != nil {
						c.script.callOnGroundItemDespawned(c.groundItems[j])
					}
				}
			}
			c.groundItemCount = groundItemCount

			objectCount := 0
			for j := 0; j < c.objectCount; j++ {
				x2 := (c.objects[j].x >> 3) - x1
				z2 := (c.objects[j].z >> 3) - z1

				if x2 == 0 && z2 == 0 {
					c.unregisterGameObject(c.objects[j])
					if c.script != nil && c.script.onObjectDespawned != nil {
						c.script.callOnObjectDespawned(c.objects[j])
					}
				} else {
					if objectCount != j {
						c.objects[objectCount].id = c.objects[j].id
						c.objects[objectCount].x = c.objects[j].x
						c.objects[objectCount].z = c.objects[j].z
						c.objects[objectCount].dir = c.objects[j].dir
					}
					objectCount++
				}
			}
			c.objectCount = objectCount

			wallObjectCount := 0
			for j := 0; j < c.wallObjectCount; j++ {
				x2 := (c.wallObjects[j].x >> 3) - x1
				z2 := (c.wallObjects[j].z >> 3) - z1

				if x2 == 0 && z2 == 0 {
					c.unregisterWallObject(c.wallObjects[j])
					if c.script != nil && c.script.onWallObjectDespawned != nil {
						c.script.callOnWallObjectDespawned(c.wallObjects[j])
					}
				} else {
					if wallObjectCount != j {
						c.wallObjects[wallObjectCount].id = c.wallObjects[j].id
						c.wallObjects[wallObjectCount].x = c.wallObjects[j].x
						c.wallObjects[wallObjectCount].z = c.wallObjects[j].z
						c.wallObjects[wallObjectCount].dir = c.wallObjects[j].dir
					}
					wallObjectCount++
				}
			}

			c.wallObjectCount = wallObjectCount
		}

	case 48: // load scenery
		for p.hasReadableBytes() {
			id := p.readShort()
			x := c.x + int(int8(p.readByte()))
			z := c.z + int(int8(p.readByte()))
			dir := p.readByteAsInt()

			count := 0
			for i := 0; i < c.objectCount; i++ {
				if c.objects[i].x == x && c.objects[i].z == z {
					c.unregisterGameObject(c.objects[i])
					if c.script != nil && c.script.onObjectDespawned != nil {
						c.script.callOnObjectDespawned(c.objects[i])
					}
				} else {
					if count != i {
						c.objects[count].id = c.objects[i].id
						c.objects[count].x = c.objects[i].x
						c.objects[count].z = c.objects[i].z
						c.objects[count].dir = c.objects[i].dir
					}
					count++
				}
			}

			c.objectCount = count

			if id != 60000 {
				c.objects[c.objectCount].id = id
				c.objects[c.objectCount].x = x
				c.objects[c.objectCount].z = z
				c.objects[c.objectCount].dir = dir

				c.registerGameObject(c.objects[c.objectCount])
				if c.script != nil && c.script.onObjectSpawned != nil {
					c.script.callOnObjectSpawned(c.objects[c.objectCount])
				}
				c.objectCount++
			}
		}
	case 91: // load wall objects
		for p.hasReadableBytes() {
			id := p.readShort()
			x := c.x + int(int8(p.readByte()))
			z := c.z + int(int8(p.readByte()))
			dir := p.readByteAsInt()

			count := 0
			for i := 0; i < c.wallObjectCount; i++ {
				x2 := c.wallObjects[i].x - x
				z2 := c.wallObjects[i].z - z
				if x2 == 0 && z2 == 0 {
					c.unregisterWallObject(c.wallObjects[i])
					if c.script != nil && c.script.onWallObjectDespawned != nil {
						c.script.callOnWallObjectDespawned(c.wallObjects[i])
					}
				} else {
					if count != i {
						c.wallObjects[count].id = c.wallObjects[i].id
						c.wallObjects[count].x = c.wallObjects[i].x
						c.wallObjects[count].z = c.wallObjects[i].z
						c.wallObjects[count].dir = c.wallObjects[i].dir
					}
					count++
				}
			}

			c.wallObjectCount = count

			if id != 60000 {
				c.wallObjects[c.wallObjectCount].id = id
				c.wallObjects[c.wallObjectCount].x = x
				c.wallObjects[c.wallObjectCount].z = z
				c.wallObjects[c.wallObjectCount].dir = dir

				c.registerWallObject(c.wallObjects[c.wallObjectCount])
				if c.script != nil && c.script.onWallObjectSpawned != nil {
					c.script.callOnWallObjectSpawned(c.wallObjects[c.wallObjectCount])
				}
				c.wallObjectCount++
			}
		}
	case 90: // update inventory item
		slot := p.readByteAsInt()
		itemID := p.readShort()
		noted := p.readByte() == 1
		amount := 1
		if itemDefs[itemID&0x7FFF].IsStackable != 0 || noted {
			amount = p.readInt()
		}
		c.inventory[slot].id = itemID & 0x7FFF
		c.inventory[slot].equipped = itemID/0x8000 == 1
		c.inventory[slot].amount = amount
		c.inventory[slot].slot = slot
		if slot >= c.inventoryCount {
			c.inventoryCount++
		}
	case 99: // ground items
		for p.hasReadableBytes() {
			needsRemoval := p.peekByte() == 255
			if needsRemoval {
				p.readByte()
				x := (c.x + int(int8(p.readByte()))) >> 3
				z := (c.z + int(int8(p.readByte()))) >> 3
				if supportBankNotes {
					p.readByte()
				}
				count := 0
				for i := 0; i < c.groundItemCount; i++ {
					dx := (c.groundItems[i].x >> 3) - x
					dz := (c.groundItems[i].z >> 3) - z
					if dx != 0 || dz != 0 {
						if i != count {
							c.groundItems[count].id = c.groundItems[i].id
							c.groundItems[count].x = c.groundItems[i].x
							c.groundItems[count].z = c.groundItems[i].z
						}
						count++
					} else {
						if c.script != nil && c.script.onGroundItemDespawned != nil {
							c.script.callOnGroundItemDespawned(c.groundItems[i])
						}
					}
				}
				c.groundItemCount = count
			} else {
				id := p.readShort()
				x := c.x + int(int8(p.readByte()))
				z := c.z + int(int8(p.readByte()))
				if supportBankNotes {
					p.readByte()
				}
				if (id & 0x8000) != 0 {
					id &= 0x7FFF

					count := 0
					for i := 0; i < c.groundItemCount; i++ {
						if c.groundItems[i].x == x && c.groundItems[i].z == z && c.groundItems[i].id == id {
							id = -123
							if c.script != nil && c.script.onGroundItemDespawned != nil {
								c.script.callOnGroundItemDespawned(c.groundItems[i])
							}
						} else {
							if i != count {
								c.groundItems[count].id = c.groundItems[i].id
								c.groundItems[count].x = c.groundItems[i].x
								c.groundItems[count].z = c.groundItems[i].z
							}
							count++
						}
					}

					c.groundItemCount = count
				} else {
					c.groundItems[c.groundItemCount].id = id
					c.groundItems[c.groundItemCount].x = x
					c.groundItems[c.groundItemCount].z = z
					if c.script != nil && c.script.onGroundItemSpawned != nil {
						c.script.callOnGroundItemSpawned(c.groundItems[c.groundItemCount])
					}
					c.groundItemCount++
				}
			}
		}
	case 234: // player visual update
		playerCount := p.readShort()
	next:
		for i := 0; playerCount > i; i++ {
			serverIndex := int(int16(p.readShort()))
			var player *player
			if serverIndex != -1 {
				player = c.playersServer[serverIndex]
			}
			updateType := p.readByte()

			switch updateType {
			case 0: // bubble
				itemID := p.readShort()
				_ = itemID
				if player != nil {
					player.skillTime = time.Now()
					player.skillingTimeout = time.Duration(float64(time.Second) * 2.4)
				}
			case 1, 6, 7: // public message
				if updateType == 1 || updateType == 7 {
					crownID := p.readInt()
					if updateType == 7 {
						muted := p.readByte()
						onTutorial := p.readByte()
						_ = muted
						_ = onTutorial
					}
					_ = crownID
					message := p.readString()
					if updateType == 7 && message == "" {
						continue next
					}
					if player != nil {
						if c.account.debug {
							fmt.Printf("[%s] Chat message[%d]: (%s) %s\n", c.user, updateType, player.username, message)
						}
						if c.script != nil && c.script.onChatMessage != nil {
							c.script.callOnChatMessage(message, player.username)
						}
					}
				} else {
					message := p.readString()
					if player != nil {
						if len(message) >= 65 {
							player.lastMessageTimeout = time.Second * 4
						} else {
							player.lastMessageTimeout = time.Second * 3
						}
						player.messageTime = time.Now()
						if c.account.debug {
							fmt.Printf("[%s] Chat message[%d]: (%s) %s\n", c.user, updateType, player.username, message)
						}
					}
				}
			case 2: // damage update
				damage := p.readByteAsInt()
				currentHP := p.readByteAsInt()
				maxHP := p.readByteAsInt()
				if player != nil {
					player.currentHP = currentHP
					player.maxHP = maxHP
					if serverIndex == c.serverIndex {
						c.currentStats[3] = currentHP
						c.baseStats[3] = maxHP
					}
					if c.script != nil && c.script.onPlayerDamaged != nil {
						c.script.callOnPlayerDamaged(damage, player)
					}
				}
			case 3: // projectile @ NPC
				projectileType := p.readShort()
				receiverServerIndex := p.readShort()
				if player != nil {
					if npc := c.npcsServer[receiverServerIndex]; npc != nil {
						if c.script != nil && c.script.onNPCProjectile != nil {
							c.script.callOnNPCProjectile(projectileType, npc, player)
						}
					}
				}
			case 4: // projectile @ player
				projectileType := p.readShort()
				shooterServerIndex := p.readShort()
				_ = projectileType
				_ = shooterServerIndex
			case 5: // appearance update
				clan := ""
				username := p.readString()
				itemCount := p.readByteAsInt()
				for i := 0; i < itemCount; i++ {
					p.readShort()
				}
				p.readByte()
				p.readByte()
				p.readByte()
				p.readByte()
				combatLevel := p.readByte()
				p.readByte()
				flag := p.readByte()
				if flag == 1 {
					clan = p.readString()
				}
				p.readByte()
				p.readByte()
				p.readByte()
				p.readInt()
				if player != nil {
					player.combatLevel = int(combatLevel)
					player.username = username
					player.clan = clan
				}
			case 8:
				heal := p.readByte()
				currentHP := p.readByteAsInt()
				maxHP := p.readByteAsInt()
				if player != nil {
					player.currentHP = currentHP
					player.maxHP = maxHP
					if serverIndex == c.serverIndex {
						c.currentStats[3] = currentHP
						c.baseStats[3] = maxHP
					}
				}
				_ = heal
			case 9:
				currentHP := p.readByteAsInt()
				maxHP := p.readByteAsInt()
				if player != nil {
					player.currentHP = currentHP
					player.maxHP = maxHP
					if serverIndex == c.serverIndex {
						c.currentStats[3] = currentHP
						c.baseStats[3] = maxHP
					}
				}
			}
		}
	case 114: // fatigue
		c.fatigue = p.readShort()
		c.accurateFatigue = (float64(p.readShort()) / 750) * 100

		if c.script != nil && c.script.onFatigueUpdate != nil {
			c.script.callOnFatigueUpdate(c.fatigue, c.accurateFatigue)
		}
	case 53: // Update inventory
		c.inventoryCount = p.readByteAsInt()

		for i := 0; i < c.inventoryCount; i++ {
			id := p.readShort()
			equipped := p.readByte() > 0

			noted := p.readByte()
			_ = noted

			var amount int
			if itemDefs[id].IsStackable != 0 {
				amount = p.readInt()
			} else {
				amount = 1
			}
			c.inventory[i].id = id
			c.inventory[i].amount = amount
			c.inventory[i].equipped = equipped
			c.inventory[i].slot = i
		}

	case 117: // sleep image
		if c.account.debug {
			fmt.Printf("[%s] Got sleep image\n", c.user)
		}
		resleep := false
		if c.sleeping {
			resleep = true
		} else {
			c.sleepingFatigue = c.fatigue
		}
		c.sleeping = true
		c.captchaBuf = p.readBytes(length)
		if resleep {
			if c.sleepingFatigue == 0 || (c.script != nil && c.sleepingFatigue <= c.script.wakeupAt) {
				c.doCaptcha(c.captchaBuf)
			}
		}
	case 104: // npc appearances
		nUpdates := p.readShort()
		for i := 0; i < nUpdates; i++ {
			sender := int(int16(p.readShort()))
			var npc *npc
			if sender != -1 {
				npc = c.npcsServer[sender]
			}
			updateType := p.readByte()
			switch updateType {
			case 1: // npc chat
				recipient := int16(p.readShort())
				if npc != nil {
					message := p.readString()
					if c.account.debug {
						fmt.Printf("[%s] NPC Message[%d:%d]: %s\n", c.user, npc.id, recipient, message)
					}
					if inIntArray(npc.id, bankersID) && strings.HasPrefix(message, "Certainly") {
						npc.messageTime = time.Unix(0, 0)
					} else {
						npc.messageTime = time.Now()
						if len(message) >= 65 {
							npc.lastMessageTimeout = time.Second * 4
						} else {
							npc.lastMessageTimeout = time.Second * 3
						}
					}
					if recipient != -1 {
						if player := c.playersServer[recipient]; player != nil {
							if c.script != nil && c.script.onNPCMessage != nil {
								c.script.callOnNPCMessage(message, npc, player)
							}
						}
					}
				}
			case 2: // npc damage
				damage := p.readByteAsInt()
				currentHP := p.readByteAsInt()
				maxHP := p.readByteAsInt()
				if npc != nil {
					npc.currentHP = currentHP
					npc.maxHP = maxHP
					if c.script != nil && c.script.onNPCDamaged != nil {
						c.script.callOnNPCDamaged(damage, npc)
					}
				}
			case 3: // projectile @ player
				sprite := p.readShort()
				receiverServerIndex := p.readShort()
				_ = sprite
				_ = receiverServerIndex
			case 4: // projectile @ npc
				sprite := p.readShort()
				shooterServerIndex := p.readShort()
				_ = shooterServerIndex
				_ = sprite
			case 5: // npc skulled
				skull := p.readByte()
				_ = skull
			case 6: // ?
				wield := p.readByte()
				wield2 := p.readByte()
				_ = wield
				_ = wield2
			case 7: //npc bubble
				itemType := p.readShort()
				_ = itemType
			}
		}
	case 156: // stats, experience, and quest points
		for i := 0; i < skillCount; i++ {
			c.currentStats[i] = p.readByteAsInt()
		}
		for i := 0; i < skillCount; i++ {
			c.baseStats[i] = p.readByteAsInt()
		}
		for i := 0; i < skillCount; i++ {
			c.experience[i] = int(uint32(p.readInt()) / 4)
		}
		c.questPoints = p.readByteAsInt()
	case 159: // update stat and xp
		skill := p.readByteAsInt()
		c.currentStats[skill] = p.readByteAsInt()
		c.baseStats[skill] = p.readByteAsInt()
		c.experience[skill] = int(uint32(p.readInt()) / 4)
	case 153: // equipment stats
		for i := 0; i < 5; i++ {
			c.equipmentStats[i] = p.readByteAsInt()
		}
	case 206: // toggle prayers
		for i := 0; i < length; i++ {
			c.prayers[i] = p.readByte() == 1
		}
	case 33: // update individual experience
		skill := p.readByteAsInt()
		c.experience[skill] = int(uint32(p.readInt()) / 4)
	case 244: // sleeping menu fatigue
		c.sleepingFatigue = p.readShort()
		if c.account.debug {
			fmt.Printf("[%s] Sleep fatigue at %d\n", c.user, c.sleepingFatigue)
		}
		if c.sleepingFatigue == 0 || (c.script != nil && c.sleepingFatigue <= c.script.wakeupAt) {
			c.doCaptcha(c.captchaBuf)
		}
	case 115: // black hole
		c.blackhole = p.readByte() != 0
	case 123: // remove inventory item
		slot := p.readByteAsInt()
		c.inventoryCount -= 1
		for i := slot; i < c.inventoryCount; i++ {
			c.inventory[i].id = c.inventory[i+1].id
			c.inventory[i].amount = c.inventory[i+1].amount
			c.inventory[i].equipped = c.inventory[i+1].equipped
			c.inventory[i].slot = i
		}
	case 84: // wake up
		c.sleeping = false
	case 129: // combat style changed
		c.combatStyle = p.readByteAsInt()
		if c.account.debug {
			fmt.Printf("[%s] Combat style: %d\n", c.user, c.combatStyle)
		}
	case 245: // show option menu
		c.optionMenuCount = p.readByteAsInt()
		for i := 0; i < c.optionMenuCount; i++ {
			c.optionMenu[i] = p.readString()
		}
		c.optionMenuVisible = true
		if c.script != nil && c.script.onOptionMenu != nil {
			c.script.callOnOptionMenu(c.optionMenu[:c.optionMenuCount])
		}
	case 252: // hide option menu
		c.optionMenuVisible = false
	case 120: // private message
		playerName := p.readString()
		p.readString()
		p.readInt()
		message := p.readHuffman()
		if c.account.debug {
			fmt.Printf("[%s] Private message [%s]: %s\n", c.user, playerName, message)
		}
		if c.script != nil && c.script.onPrivateMessage != nil {
			c.script.callOnPrivateMessage(message, playerName)
		}
	case 87: // private message sent
		playerName := p.readString()
		message := p.readHuffman()
		if c.account.debug {
			fmt.Printf("[%s] You private message [%s]: %s\n", c.user, playerName, message)
		}
	case 249: // bank update
		slot := p.readByteAsInt()
		id := p.readShort()
		amount := p.readInt()
		if amount == 0 {
			if slot+1 >= len(c.bankItems) {
				c.bankItems = c.bankItems[:slot]
			} else {
				c.bankItems = append(c.bankItems[:slot], c.bankItems[slot+1:]...)
			}
			c.bankItemCount--
			return
		}

		if len(c.bankItems) <= slot {
			c.bankItems = append(c.bankItems, &bankItem{
				id:     id,
				amount: amount,
			})
			c.bankItemCount++
		} else {
			c.bankItems[slot].id = id
			c.bankItems[slot].amount = amount
		}
	case 42: // open bank
		c.banking = true
		c.bankItemCount = p.readShort()
		c.maxBankItemCount = p.readShort()
		c.bankItems = c.bankItems[:0]
		for i := 0; i < c.bankItemCount; i++ {
			c.bankItems = append(c.bankItems, &bankItem{
				id:     p.readShort(),
				amount: p.readInt(),
			})
		}
	case 203: // close bank
		c.banking = false
	case 92: // trade init
		c.tradeRecipient = p.readShort()
		c.tradeScreen1Active = true
		c.tradeScreen2Active = false
		c.recipientTradeCount = 0
		c.myTradeCount = 0
		c.tradeAccepted = false
		c.tradeRecipientAccepted = false
		c.tradeConfirmAccepted = false
		c.recipientConfirmItemCount = 0
		c.myTradeConfirmItemCount = 0
	case 15: // trade accepted
		c.tradeAccepted = p.readByte() > 0
	case 162: // trade recipient accepted
		c.tradeRecipientAccepted = p.readByte() > 0
	case 20: // trade confirm screen
		c.tradeScreen1Active = false
		c.tradeScreen2Active = true
		c.tradeConfirmAccepted = false
		p.readString() // recipient name
		c.recipientConfirmItemCount = p.readByteAsInt()
		for i := 0; i < c.recipientConfirmItemCount; i++ {
			c.recipientConfirmItems[i] = &tradeItem{
				id:     p.readShort(),
				amount: p.readInt(),
			}
		}
		c.myTradeConfirmItemCount = p.readByteAsInt()
		for i := 0; i < c.myTradeConfirmItemCount; i++ {
			c.myTradeConfirmItems[i] = &tradeItem{
				id:     p.readShort(),
				amount: p.readInt(),
			}
		}
	case 128: // close trade
		c.tradeScreen1Active = false
		c.tradeScreen2Active = false
		c.recipientTradeCount = 0
		c.myTradeCount = 0
		c.tradeAccepted = false
		c.tradeRecipientAccepted = false
		c.tradeConfirmAccepted = false
		c.recipientConfirmItemCount = 0
		c.myTradeConfirmItemCount = 0
	case 97: // update trade offers
		c.recipientTradeCount = p.readByteAsInt()
		for i := 0; i < c.recipientTradeCount; i++ {
			id := p.readShort()
			if c.recipientTradeItems[i] != nil && id == c.recipientTradeItems[i].id {
				c.recipientTradeItems[i].amount = p.readInt()
			} else {
				c.recipientTradeItems[i] = &tradeItem{
					id:     id,
					amount: p.readInt(),
				}
			}
		}
		c.myTradeCount = p.readByteAsInt()
		for i := 0; i < c.myTradeCount; i++ {
			id := p.readShort()
			if c.myTradeItems[i] != nil && id == c.myTradeItems[i].id {
				c.myTradeItems[i].amount = p.readInt()
			} else {
				c.myTradeItems[i] = &tradeItem{
					id:     id,
					amount: p.readInt(),
				}
			}
		}
	case 101: // shop open
		c.shopItemCount = p.readByteAsInt()
		p.readByte()                 // shop type
		sellMod := p.readByteAsInt() // sell mod
		buyMod := p.readByteAsInt()  // buy mod
		pmult := p.readByteAsInt()   // price multiplier

		for i := 0; i < c.shopItemCount; i++ {
			id := p.readShort()
			basePrice := itemDefs[id].BasePrice
			amount := p.readShort()
			shopPrice := p.readShort()
			if c.shopItems[i] != nil && c.shopItems[i].id == id {
				c.shopItems[i].amount = amount
				c.shopItems[i].buyPrice = calculatePrice(basePrice, shopPrice, buyMod, pmult, amount, 1, true)
				c.shopItems[i].sellPrice = calculatePrice(basePrice, shopPrice, sellMod, pmult, amount, 1, true)
			} else {
				c.shopItems[i] = &shopItem{
					id:        id,
					amount:    amount,
					buyPrice:  calculatePrice(basePrice, shopPrice, buyMod, pmult, amount, 1, true),
					sellPrice: calculatePrice(basePrice, shopPrice, sellMod, pmult, amount, 1, true),
				}
			}
		}
		c.shopping = true
	case 137: // shop close
		c.shopping = false
	case 131: // Server message
		p.readInt()             // icon sprite
		msgType := p.readByte() // message type
		infoContained := p.readByte()
		msg := p.readString()
		var senderName string
		if (infoContained & 1) != 0 {
			senderName = p.readString() // sender name
			p.readString()              // redundant sender name
		}
		if (infoContained & 2) != 0 {
			p.readString()
		}
		switch msgType {
		case 0:
			if c.account.debug {
				fmt.Printf("[%s] Server message: %s\n", c.user, msg)
			}
			if c.script != nil && c.script.onServerMessage != nil {
				c.script.callOnServerMessage(msg)
			}
		case 1:
			if c.account.debug {
				fmt.Printf("[%s] Private message from [%s]: %s\n", c.user, senderName, msg)
			}
			if c.script != nil && c.script.onPrivateMessage != nil {
				c.script.callOnPrivateMessage(msg, senderName)
			}
		case 3:
			if c.account.debug {
				fmt.Printf("[%s] Quest message: %s\n", c.user, msg)
			}
			if c.script != nil && c.script.onServerMessage != nil {
				c.script.callOnServerMessage(msg)
			}
		case 4:
			if c.account.debug {
				fmt.Printf("[%s] Chat message from [%s]: %s\n", c.user, senderName, msg)
			}
			if c.script != nil && c.script.onChatMessage != nil {
				c.script.callOnChatMessage(msg, senderName)
			}
		case 5:
			if c.account.debug {
				fmt.Printf("[%s] Friend status: %s\n", c.user, msg)
			}
		case 6:
			if c.account.debug {
				fmt.Printf("[%s] [%s] wishes to trade with you\n", c.user, senderName)
			}
			if c.script != nil && c.script.onTradeRequest != nil {
				c.script.callOnTradeRequest(senderName)
			}
		case 7:
			if c.account.debug {
				fmt.Printf("[%s] Inventory: %s\n", c.user, msg)
			}
			if c.script != nil && c.script.onServerMessage != nil {
				c.script.callOnServerMessage(msg)
			}
		case 8:
			if c.account.debug {
				fmt.Printf("[%s] Global message from [%s]: %s\n", c.user, senderName, msg)
			}
		}
	case 149: // friend update
		name := p.readString()
		formerName := p.readString()
		onlineStatus := p.readByteAsInt()
		rename := (1 & onlineStatus) != 0
		online := (4 & onlineStatus) != 0
		if online {
			p.readString() // world
		}
		for i := 0; i < c.friendListCount; i++ {
			if c.friendList[i].username == name || c.friendList[i].username == formerName {
				if rename && c.friendList[i].username == formerName {
					c.friendList[i].username = name
				}
				c.friendList[i].online = online
				return
			}
		}
		c.friendList[c.friendListCount] = &friend{
			username: name,
			online:   online,
		}
		c.friendListCount++
	case 109: // ignore list
		c.ignoreListCount = p.readByteAsInt()
		for i := 0; i < c.ignoreListCount; i++ {
			c.ignoreList[i].arg0 = p.readString()
			c.ignoreList[i].username = p.readString()
			c.ignoreList[i].arg1 = p.readString()
			c.ignoreList[i].old = p.readString()
		}
		if c.loading {
			c.loading = false
			if c.script != nil && c.script.onLoad != nil {
				c.script.callOnLoad()
			}
			c.initChans()
			if c.onLoaded != nil {
				c.onLoaded()
			}
		}
	case 183: // can't logout response packet
		if c.account.debug {
			fmt.Printf("[%s] Logout Request: Cannot logout\n", c.user)
		}
	case 5: // quest stage info
		updateQuestType := p.readByte()
		if updateQuestType == 0 {
			c.questCount = p.readByteAsInt()
			for i := 0; i < c.questCount; i++ {
				questID := p.readInt()
				questStage := p.readInt()
				questName := p.readString()

				c.quests[questID] = &quest{
					id:    questID,
					name:  questName,
					stage: int(int32(questStage)),
				}
			}
		} else if updateQuestType == 1 {
			c.quests[p.readInt()].stage = int(int32(p.readInt()))
		}
	case 237: // update ignore name change
		arg0 := p.readString()
		replace := p.readString()
		if len(replace) == 0 {
			replace = arg0
		}
		arg1 := p.readString()
		find := p.readString()
		if len(find) == 0 {
			find = arg0
		}
		rename := p.readByte() == 1
		for i := 0; i < c.ignoreListCount; i++ {
			if rename {
				if c.ignoreList[i].username == find {
					c.ignoreList[i].arg0 = arg0
					c.ignoreList[i].username = replace
					c.ignoreList[i].arg1 = arg1
					c.ignoreList[i].old = find
					return
				}
			} else if c.ignoreList[i].username == replace {
				return
			}
		}

		if rename {
			return
		}

		c.ignoreList[c.ignoreListCount].arg0 = arg0
		c.ignoreList[c.ignoreListCount].username = replace
		c.ignoreList[c.ignoreListCount].arg1 = arg1
		c.ignoreList[c.ignoreListCount].old = find
		c.ignoreListCount++
	case 83: // death
		if c.account.debug {
			fmt.Printf("[%s] Died\n", c.user)
		}
		if c.script != nil && c.script.onDeath != nil {
			c.script.callOnDeath()
		}
	case 59: // show appearance change
		c.appearanceChange = true
	case 194: // sleep word incorrect
		if c.account.debug {
			fmt.Printf("[%s] Sleep word incorrect\n", c.user)
		}
	case 52: // system update
		c.systemUpdate = true
		seconds := int(float64(p.readShort()) / 50 * 32)
		if c.script != nil && c.script.onSystemUpdate != nil {
			c.script.callOnSystemUpdate(seconds)
		}
	case 36: // telegrab bubble
	case 222: // welcome box
	case 19: // server configs
	case 4: // close connection
	case 51: // privacy settings
	case 240: // game settings
	case 111: // completed tutorial
	case 182: // show welcome
	case 147: // send kills
	case 113: // send ironman
	case 204: // play sound
	case 250: // unlocked appearances?
	default:
		if c.account.debug {
			fmt.Printf("[%s] Unhandled packet [OPCODE=%d, LENGTH=%d]\n", c.user, opcode, length)
		}
	}
}
