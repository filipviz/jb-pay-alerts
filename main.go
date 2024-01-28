package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
)

const (
	MINUTES_BETWEEN_CHECKS = 5
	IPFS_ENDPOINT          = "https://jbm.infura-ipfs.io/ipfs/"
)

type GraphQLRequest struct {
	Query string `json:"query"`
}

type PayEvent struct {
	Pv          string `json:"pv"`
	ProjectId   int    `json:"projectId"`
	Amount      string `json:"amount"`
	AmountUSD   string `json:"amountUSD"`
	Timestamp   int    `json:"timestamp"`
	Beneficiary string `json:"beneficiary"`
	Note        string `json:"note"`
	TxHash      string `json:"txHash"`
	Project     struct {
		MetadataUri string `json:"metadataUri"`
		Handle      string `json:"handle"`
	} `json:"project"`
}

type PayEventsResponse struct {
	Data struct {
		PayEvents []PayEvent `json:"payEvents"`
	} `json:"data"`
}

type Project struct {
	Pv          string `json:"pv"`
	Handle      string `json:"handle"`
	ProjectId   int    `json:"projectId"`
	MetadataUri string `json:"metadataUri"`
	Creator     string `json:"creator"`
	Owner       string `json:"owner"`
	InitEvents  []struct {
		TxHash string `json:"txHash"`
	} `json:"initEvents"`
}

type ProjectsResponse struct {
	Data struct {
		Projects []Project `json:"projects"`
	}
}

type Metadata struct {
	Name           string `json:"name"`
	InfoUri        string `json:"infoUri"`
	LogoUri        string `json:"logoUri"`
	Description    string `json:"description"`
	ProjectTagline string `json:"projectTagline"`
}

type MetadataCacheValue struct {
	MetadataIPFSUri string
	Metadata        Metadata
	ready           chan struct{} // Closed when metadata is ready.
}

type MetadataCache struct {
	sync.Mutex // Protects the map.
	Map        map[string]*MetadataCacheValue
}

func main() {
	// Read and check environment variables.
	if err := godotenv.Load(".env"); err != nil {
		log.Fatalln("No .env file found")
	}
	discordToken := os.Getenv("DISCORD_TOKEN")
	if discordToken == "" {
		log.Fatalln("Could not find DISCORD_TOKEN in .env file")
	}
	subgraphUrl := os.Getenv("SUBGRAPH_URL")
	if subgraphUrl == "" {
		log.Fatalln("SUBGRAPH_URL must be set")
	}

	// Read the config file.
	configBytes, err := os.ReadFile("config.json")
	if err != nil {
		log.Fatalf("Failed to read config file: %v\n", err)
	}
	var config map[string][]string
	err = json.Unmarshal(configBytes, &config)
	if err != nil {
		log.Fatalf("Failed to parse config file: %v\n", err)
	}

	// Invert the config map
	projectsToChannels := make(map[string][]string)
	for channel, projects := range config {
		for _, project := range projects {
			projectsToChannels[project] = append(projectsToChannels[project], channel)
		}
	}

	// Initialize the metadata cache (map of project IDs to `MetadataCacheValue`s)
	metadataCache := MetadataCache{
		Map: make(map[string]*MetadataCacheValue),
	}

	log.Println("Started.")

	// Start a 5 minute ticker.
	// Run loop off of the main thread.
	go func() {
		previousTime := time.Now()
		t := time.NewTicker(MINUTES_BETWEEN_CHECKS * time.Minute)
		for {
			currentTime := <-t.C
			// Every 5 minutes, check the subgraph for new pay events.
			// If there are new pay events, send them to the appropriate places.
			payResp, err := getPayEvents(previousTime, subgraphUrl)
			if err != nil {
				log.Printf("Failed to get pay events: %v\n", err)
				continue
			}

			projResp, err := getNewProjects(previousTime, subgraphUrl)
			if err != nil {
				log.Printf("Failed to get new projects: %v\n", err)
				continue
			}

			if len(payResp.Data.PayEvents) > 0 || len(projResp.Data.Projects) > 0 {
				// If there are pay events, initialize a new discord session.
				s, err := discordgo.New("Bot " + discordToken)
				if err != nil {
					log.Printf("Failed to create discord session: %v\n", err)
					continue
				}

				// s.LogLevel = discordgo.LogWarning
				s.ShouldRetryOnRateLimit = true
				if err = s.Open(); err != nil {
					log.Printf("Failed to open discord session: %v\n", err)
					continue
				} else {
					log.Println("Discord session opened")
				}

				for _, payEvent := range payResp.Data.PayEvents {
					go func(p PayEvent) {
						projectIdStr := fmt.Sprintf("%d", p.ProjectId)
						metadata := memoizedMetadata(&metadataCache, projectIdStr, p.Project.MetadataUri, p.Project.Handle, p.Pv)

						// Check for wildcard channels, and notify them if they exist.
						wChannels, wExists := projectsToChannels["*"]
						if wExists {
							go sendPayEventToDiscord(p, wChannels, metadata, s)
						}

						// Check for channels for this project, and notify them if they exist.
						pChannels, pExists := projectsToChannels[projectIdStr]
						if pExists {
							go sendPayEventToDiscord(p, pChannels, metadata, s)
						}
					}(payEvent)
				}

				for _, newProject := range projResp.Data.Projects {
					go func(p Project) {
						projectIdStr := fmt.Sprintf("%d", p.ProjectId)
						metadata := memoizedMetadata(&metadataCache, projectIdStr, p.MetadataUri, p.Handle, p.Pv)

						channels, exists := projectsToChannels["new"]
						if exists {
							go sendNewProjectToDiscord(p, channels, metadata, s)
						}
					}(newProject)
				}
			}

			previousTime = currentTime
		}
	}()

	// Wait for signal to stop.
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, os.Interrupt, syscall.SIGTERM, syscall.SIGINT, syscall.SIGSEGV, syscall.SIGHUP)
	<-sc
}

// Get the pay events from the Subgraph URL since the given time.
func getPayEvents(since time.Time, subgraphUrl string) (*PayEventsResponse, error) {
	reqBody := GraphQLRequest{
		Query: `{
			payEvents(
			  first: 1000
			  orderBy: timestamp
			  orderDirection: asc
			  where: {timestamp_gt:` +
			fmt.Sprintf("%d", since.Unix()) +
			`}) {
				pv
				projectId
				amount
				amountUSD
				timestamp
				beneficiary
				note
				txHash
				project {
					metadataUri
					handle
				}
			}
		}`,
	}
	reqBodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("error marshalling request body: %w", err)
	}
	resp, err := http.Post(subgraphUrl, "application/json", bytes.NewBuffer(reqBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("error posting request to subgraph: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading subgraph response body: %w", err)
	}

	var p PayEventsResponse
	if err = json.Unmarshal(respBody, &p); err != nil {
		return nil, fmt.Errorf("error unmarshalling response body: %w", err)
	}

	return &p, nil
}

func getNewProjects(since time.Time, subgraphUrl string) (*ProjectsResponse, error) {
	reqBody := GraphQLRequest{
		Query: `{
			projects(
			  first: 1000
			  orderBy: createdAt
			  orderDirection: desc
			  where: {createdAt_gt:` +
			fmt.Sprintf("%d", since.Unix()) +
			`}
			) {
			  pv
			  handle
			  projectId
			  metadataUri
			  creator
			  owner    
			  initEvents(first: 1, orderBy: timestamp, orderDirection: asc) {
				txHash
			  }
			}
		  }`,
	}
	reqBodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("error marshalling request body: %w", err)
	}
	resp, err := http.Post(subgraphUrl, "application/json", bytes.NewBuffer(reqBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("error posting request to subgraph: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading subgraph response body: %w", err)
	}

	var p ProjectsResponse
	if err = json.Unmarshal(respBody, &p); err != nil {
		return nil, fmt.Errorf("error unmarshalling response body: %w", err)
	}

	return &p, nil
}

func sendPayEventToDiscord(p PayEvent, channels []string, m Metadata, s *discordgo.Session) {
	for _, channel := range channels {
		_, err := s.ChannelMessageSendEmbed(channel, formatPayEvent(p, m))
		if err != nil {
			log.Printf("Failed to send message to channel %s: %v\n", channel, err)
		}
	}
}

func sendNewProjectToDiscord(p Project, channels []string, m Metadata, s *discordgo.Session) {
	for _, channel := range channels {
		_, err := s.ChannelMessageSendEmbed(channel, formatProjectMessage(p, m))
		if err != nil {
			log.Printf("Failed to send message to channel %s: %v\n", channel, err)
		}
	}
}

func formatProjectMessage(p Project, m Metadata) *discordgo.MessageEmbed {
	fields := make([]*discordgo.MessageEmbedField, 0, 4)

	if m.ProjectTagline != "" {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Tagline",
			Value:  m.ProjectTagline,
			Inline: false,
		})
	}

	if p.Creator != "" {
		var creator string
		if ensName, err := getEnsForAddress(p.Creator); err == nil {
			creator = ensName
		} else {
			creator = p.Creator
		}

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Creator",
			Value:  fmt.Sprintf("[%s](https://juicebox.money/account/%s)", creator, p.Creator),
			Inline: true,
		})
	}

	if p.Creator != p.Owner {
		var owner string
		if ensName, err := getEnsForAddress(p.Owner); err == nil {
			owner = ensName
		} else {
			owner = p.Owner
		}

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Owner",
			Value:  fmt.Sprintf("[%s](https://juicebox.money/account/%s)", owner, p.Owner),
			Inline: true,
		})
	}

	if len(p.InitEvents) != 0 && p.InitEvents[0].TxHash != "" {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Transaction",
			Value:  fmt.Sprintf("[Etherscan](https://etherscan.io/tx/%s)", p.InitEvents[0].TxHash),
			Inline: true,
		})
	}

	projectLink := ""
	if p.Pv == "2" {
		projectLink = fmt.Sprintf("https://juicebox.money/v2/p/%d", p.ProjectId)
	} else if p.Pv == "1" {
		projectLink = fmt.Sprintf("https://juicebox.money/p/%s", p.Handle)
	}

	return &discordgo.MessageEmbed{
		Title:     fmt.Sprintf("🆕🆕🆕 NEW PROJECT: %s", m.Name),
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: getUrlFromUri(m.LogoUri)},
		URL:       projectLink,
		Color:     rand.Intn(0xffffff + 1),
		Fields:    fields,
	}
}

func formatPayEvent(p PayEvent, m Metadata) *discordgo.MessageEmbed {
	fields := make([]*discordgo.MessageEmbedField, 0, 4)
	if p.Note != "" {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Note",
			Value:  p.Note,
			Inline: false,
		})
	}

	if p.Amount != "" && p.AmountUSD != "" {
		amountStr, err := parseFixedPointString(p.Amount, 18, -1)
		amountUsdStr, usdErr := parseFixedPointString(p.AmountUSD, 18, 2)
		if err == nil && usdErr == nil {
			fields = append(fields, &discordgo.MessageEmbedField{
				Name:   "Amount",
				Value:  fmt.Sprintf("%s ETH ($%s USD)", amountStr, amountUsdStr),
				Inline: true,
			})
		} else {
			log.Printf("Failed to parse amount string: %v\n", err)
		}
	}

	if p.Beneficiary != "" {
		var beneficiary string
		if ensName, err := getEnsForAddress(p.Beneficiary); err == nil {
			beneficiary = ensName
		} else {
			beneficiary = p.Beneficiary
		}

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Beneficiary",
			Value:  fmt.Sprintf("[%s](https://juicebox.money/account/%s)", beneficiary, p.Beneficiary),
			Inline: true,
		})
	}

	if p.TxHash != "" {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Transaction",
			Value:  fmt.Sprintf("[Etherscan](https://etherscan.io/tx/%s)", p.TxHash),
			Inline: true,
		})
	}

	projectLink := ""
	if p.Pv == "2" {
		projectLink = fmt.Sprintf("https://juicebox.money/v2/p/%d", p.ProjectId)
	} else if p.Pv == "1" {
		projectLink = fmt.Sprintf("https://juicebox.money/p/%s", p.Project.Handle)
	}

	return &discordgo.MessageEmbed{
		Title:     fmt.Sprintf("Payment to %s", m.Name),
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: getUrlFromUri(m.LogoUri)},
		URL:       projectLink,
		Color:     rand.Intn(0xffffff + 1),
		Fields:    fields,
	}
}

func parseFixedPointString(s string, decimals int64, precision int) (string, error) {
	bf, ok := new(big.Float).SetString(s)
	if !ok {
		return "", fmt.Errorf("error parsing fixed decimal string: %s", s)
	}

	divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(decimals), nil)
	floatDivisor := new(big.Float).SetInt(divisor)
	result := new(big.Float).Quo(bf, floatDivisor)

	return result.Text('f', precision), nil
}

func getMetadataForUri(uri string) (*Metadata, error) {
	metadataUrl := getUrlFromUri(uri)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataUrl, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request to IPFS: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error getting metadata from IPFS: %w", err)
	}

	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading IPFS response body: %w", err)
	}

	var m Metadata
	if err = json.Unmarshal(respBody, &m); err != nil {
		return nil, fmt.Errorf("error unmarshalling IPFS response body: %w", err)
	}

	return &m, nil
}

type ENSResponse struct {
	Name string `json:"name"`
}

func getEnsForAddress(address string) (string, error) {
	// Make sure address is a valid address
	if len(address) != 42 || address[0:2] != "0x" {
		return "", fmt.Errorf("invalid address")
	}

	resp, err := http.Get("https://api.ensideas.com/ens/resolve/" + address)
	if err != nil {
		return "", fmt.Errorf("error getting ENS name: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading ENS response body: %w", err)
	}

	var r ENSResponse
	if err = json.Unmarshal(respBody, &r); err != nil {
		return "", fmt.Errorf("error unmarshalling ENS response body: %w", err)
	}

	if r.Name == "" {
		return "", fmt.Errorf("no ENS name found")
	}

	return r.Name, nil
}

func getUrlFromUri(uri string) string {
	// Check if the URI is already a URL.
	if len(uri) >= 4 && uri[0:4] == "http" {
		oldEndpoint := "https://jbx.mypinata.cloud/ipfs/"
		if len(uri) >= len(oldEndpoint) && uri[0:len(oldEndpoint)] == oldEndpoint {
			uri = IPFS_ENDPOINT + uri[len(oldEndpoint)-1:]
		}

		return uri
	}

	// Check if the URI starts with ipfs://
	if len(uri) >= 7 && uri[0:7] == "ipfs://" {
		uri = uri[7:]
	}

	return IPFS_ENDPOINT + uri
}

func memoizedMetadata(m *MetadataCache, projectId string, metadataUri string, handle string, pv string) Metadata {
	m.Lock()
	cacheValue := m.Map[projectId]

	// If the cache value exists but the IPFS URI has changed, invalidate the currently cached value.
	if cacheValue != nil {
		select {
		case <-cacheValue.ready:
			if cacheValue.MetadataIPFSUri != metadataUri {
				cacheValue = nil
			}
		default:
		}
	}

	// If there is no valid cache value, create a new one.
	if cacheValue == nil {
		// Create placeholder metadata (and new ready chan).
		cacheValue = createPlaceholderCacheValue(projectId, handle, pv)
		m.Map[projectId] = cacheValue
		m.Unlock()

		if metadataUri != "" {
			// If there is a metadata URI, get the metadata from IPFS.
			newMetadata, err := getMetadataForUri(metadataUri)
			if err != nil {
				log.Printf("Failed to fetch metadata for project %s (using placeholder): %v\n", projectId, err)
				close(cacheValue.ready)
			} else {
				cacheValue.Metadata = *newMetadata
				close(cacheValue.ready)
			}
		}
	} else {
		m.Unlock()
		<-cacheValue.ready
	}

	return cacheValue.Metadata
}

func createPlaceholderCacheValue(projectId string, handle string, pv string) *MetadataCacheValue {
	metadata := &Metadata{Name: fmt.Sprintf("Project %s", projectId)}
	if pv == "2" {
		metadata.InfoUri = fmt.Sprintf("https://juicebox.money/v2/p/%s", projectId)
	} else if pv == "1" {
		metadata.Name += "(v1)"
		metadata.InfoUri = fmt.Sprintf("https://juicebox.money/p/%s", handle)
	}

	return &MetadataCacheValue{
		MetadataIPFSUri: "",
		Metadata:        *metadata,
		ready:           make(chan struct{}),
	}
}
