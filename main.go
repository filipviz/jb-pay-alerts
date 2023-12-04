package main

import (
	"bytes"
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
	IPFS_ENDPOINT          = "https://ipfs.io/ipfs/"
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

type SubgraphResponse struct {
	Data struct {
		PayEvents []PayEvent `json:"payEvents"`
	} `json:"data"`
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
}

type MetadataCache struct {
	sync.RWMutex
	Map map[string]MetadataCacheValue
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
		Map: make(map[string]MetadataCacheValue),
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
			resp, err := getPayEvents(previousTime, subgraphUrl)
			if err != nil {
				log.Printf("Failed to get pay events: %v\n", err)
				continue
			}

			if len(resp.Data.PayEvents) > 0 {
				// If there are pay events, initialize a new discord session.
				s, err := discordgo.New("Bot " + discordToken)
				if err != nil {
					log.Printf("Failed to create discord session: %v\n", err)
					continue
				}

				s.LogLevel = discordgo.LogWarning
				s.ShouldRetryOnRateLimit = true
				if err = s.Open(); err != nil {
					log.Printf("Failed to open discord session: %v\n", err)
					continue
				} else {
					log.Println("Discord session opened")
				}

				for _, payEvent := range resp.Data.PayEvents {
					go func(p PayEvent) {
						projectIdStr := fmt.Sprintf("%d", p.ProjectId)
						metadataCache.RLock()
						cacheValue, exists := metadataCache.Map[projectIdStr]
						metadataCache.RUnlock()
						// If the metadata for this project is not in the cache, or the metadata IPFS URI has changed, update the cache.
						if !exists || cacheValue.MetadataIPFSUri != p.Project.MetadataUri {
							if p.Project.MetadataUri == "" {
								// If there is no metadata URI, create placeholder metadata.
								newCacheValue := createPlaceholderCacheValue(p)
								cacheValue = newCacheValue
								metadataCache.Lock()
								metadataCache.Map[projectIdStr] = newCacheValue
								metadataCache.Unlock()
							} else {
								// If there is a metadata URI, get the metadata from IPFS.
								newMetadata, err := getMetadataForUri(p.Project.MetadataUri)
								if err != nil {
									log.Printf("Failed to get metadata for project %s: %v\n", projectIdStr, err)
									if exists {
										log.Printf("Using cached metadata for project %s.\n", projectIdStr)
									} else {
										log.Printf("No cached metadata. Creating placeholder metadata for project %s.\n", projectIdStr)
										newCacheValue := createPlaceholderCacheValue(p)
										cacheValue = newCacheValue
										metadataCache.Lock()
										metadataCache.Map[projectIdStr] = newCacheValue
										metadataCache.Unlock()
									}
								} else {
									newCacheValue := MetadataCacheValue{
										MetadataIPFSUri: p.Project.MetadataUri,
										Metadata:        *newMetadata,
									}
									cacheValue = newCacheValue
									metadataCache.Lock()
									metadataCache.Map[projectIdStr] = newCacheValue
									metadataCache.Unlock()
								}
							}
						}

						// Check for wildcard channels, and notify them if they exist.
						wChannels, wExists := projectsToChannels["*"]
						if wExists {
							go sendPayEventToDiscord(p, wChannels, cacheValue.Metadata, s)
						}

						// Check for channels for this project, and notify them if they exist.
						pChannels, pExists := projectsToChannels[projectIdStr]
						if pExists {
							go sendPayEventToDiscord(p, pChannels, cacheValue.Metadata, s)
						}
					}(payEvent)
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
func getPayEvents(since time.Time, subgraphUrl string) (*SubgraphResponse, error) {
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

	var s SubgraphResponse
	if err = json.Unmarshal(respBody, &s); err != nil {
		return nil, fmt.Errorf("error unmarshalling response body: %w", err)
	}

	return &s, nil
}

func sendPayEventToDiscord(p PayEvent, channels []string, m Metadata, s *discordgo.Session) {
	for _, channel := range channels {
		_, err := s.ChannelMessageSendEmbed(channel, formatPayEvent(p, m))
		if err != nil {
			log.Printf("Failed to send message to channel %s: %v\n", channel, err)
		}
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
	resp, err := http.Get(metadataUrl)
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

func createPlaceholderCacheValue(p PayEvent) MetadataCacheValue {
	metadata := new(Metadata)
	metadata.Name = fmt.Sprintf("Project %d", p.ProjectId)
	projectLink := ""
	if p.Pv == "2" {
		projectLink = fmt.Sprintf("https://juicebox.money/v2/p/%d", p.ProjectId)
	} else if p.Pv == "1" {
    metadata.Name += "(v1)"
    projectLink = fmt.Sprintf("https://juicebox.money/p/%s", p.Project.Handle)
  }
	metadata.InfoUri = projectLink

	return MetadataCacheValue{
		MetadataIPFSUri: "",
		Metadata:        *metadata,
	}
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
		return uri
	}

	// Check if the URI starts with ipfs://
	if len(uri) >= 7 && uri[0:7] == "ipfs://" {
		uri = uri[7:]
	}

	return IPFS_ENDPOINT + uri
}
