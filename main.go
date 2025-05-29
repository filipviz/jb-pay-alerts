package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
)

const (
	MINUTES_BETWEEN_CHECKS = 5
	IPFS_ENDPOINT          = "https://jbm.infura-ipfs.io/ipfs/"
)

func main() {
	// Read and check environment variables.
	if err := godotenv.Load(".env"); err != nil {
		log.Fatalln("No .env file found")
	}
	discordToken := os.Getenv("DISCORD_TOKEN")
	if discordToken == "" {
		log.Fatalln("Could not find DISCORD_TOKEN in .env file")
	}
	// The subgraph provides events for Juicebox v1, v2, and v3.
	subgraphUrl := os.Getenv("SUBGRAPH_URL")
	if subgraphUrl == "" {
		log.Fatalln("Could not find SUBGRAPH_URL in .env file")
	}
	// Bendystraw provides events for Juicebox v4.
	bendystrawUrl := os.Getenv("BENDYSTRAW_URL")
	if bendystrawUrl == "" {
		log.Fatalln("Could not find BENDYSTRAW_URL in .env file")
	}
	// When testing, we log events from the past 14 days then exit
	testing := os.Getenv("TESTING") == "1"

	// Read the config file.
	configBytes, err := os.ReadFile("config.json")
	if err != nil {
		log.Fatalf("Failed to read config file: %v\n", err)
	}
	// config is a map of channel IDs to notification types - see README.md
	var config map[string][]string
	err = json.Unmarshal(configBytes, &config)
	if err != nil {
		log.Fatalf("Failed to parse config file: %v\n", err)
	}

	// Initialize the metadata cache (map of project IDs to `MetadataCacheValue`s)
	metadataCache := MetadataCache{
		Map: make(map[string]*MetadataCacheValue),
	}

	// Initialize, configure, and open our discord session.
	s, err := discordgo.New("Bot " + discordToken)
	if err != nil {
		log.Fatalf("Failed to create discord session: %v\n", err)
	}
	s.ShouldRetryOnRateLimit = true
	s.ShouldReconnectOnError = true
	s.AddHandler(func(s *discordgo.Session, d *discordgo.Disconnect) {
		log.Println("Discord disconnected, reconnecting...")
	})
	if err = s.Open(); err != nil {
		log.Fatalf("Failed to open discord session: %v\n", err)
	}
	defer s.Close()
	log.Println("Discord session opened")

	// We log events which occurred since previousTime, then update previousTime to now.
	previousTime := time.Now()
	if testing {
		// When testing, we send events from the past 14 days then exit
		previousTime = time.Now().AddDate(0, 0, -14)
		log.Printf("Testing mode: checking events from the past 14 days (since %v)\n", previousTime)
	}
	
	// processEvents fetches, processes, and sends new events.
	processEvents := func() {
		// Fetch v3 events (subgraph)
		payResp, err := v3PayEvents(previousTime, subgraphUrl)
		if err != nil {
			log.Printf("Failed to get v3 pay events: %v\n", err)
		}
		projResp, err := v3NewProjects(previousTime, subgraphUrl)
		if err != nil {
			log.Printf("Failed to get new v3 projects: %v\n", err)
		}
		
		// Fetch v4 events (bendystraw)
		payRespV4, err := v4PayEvents(previousTime, bendystrawUrl)
		if err != nil {
			log.Printf("Failed to get v4 pay events: %v\n", err)
		}
		projRespV4, err := v4NewProjects(previousTime, bendystrawUrl)
		if err != nil {
			log.Printf("Failed to get new v4 projects: %v\n", err)
		}

		// Process v3 events
		if payResp != nil && len(payResp.Data.PayEvents) > 0 {
			log.Printf("Found %d v3 pay events", len(payResp.Data.PayEvents))
			for _, payEvent := range payResp.Data.PayEvents {
				processV3PayEvent(payEvent, config, &metadataCache, s)
			}
		}
		if projResp != nil && len(projResp.Data.Projects) > 0 {
			log.Printf("Found %d new v3 projects", len(projResp.Data.Projects))
			for _, newProject := range projResp.Data.Projects {
				processV3Project(newProject, config, &metadataCache, s)
			}
		}

		// Process v4 events
		if payRespV4 != nil && len(payRespV4.Data.PayEvents.Items) > 0 {
			log.Printf("Found %d v4 pay events", len(payRespV4.Data.PayEvents.Items))
			for _, payEvent := range payRespV4.Data.PayEvents.Items {
				processV4PayEvent(payEvent, config, &metadataCache, s)
			}
		}
		if projRespV4 != nil && len(projRespV4.Data.Projects.Items) > 0 {
			log.Printf("Found %d new v4 projects", len(projRespV4.Data.Projects.Items))
			groupedProjects := groupCrossChainV4Projects(projRespV4.Data.Projects.Items)
			for _, projectGroup := range groupedProjects {
				processV4ProjectGroup(projectGroup, config, &metadataCache, s)
			}
		}
	}
	
	if testing {
		// In testing mode, run once immediately and exit
		log.Println("Running event processing once in testing mode...")
		processEvents()
		log.Println("Waiting for Discord messages to send...")
		time.Sleep(60 * time.Second)
		log.Println("Testing complete, exiting...")
		return
	}
	
	// Blocking loop to check for events every MINUTES_BETWEEN_CHECKS minutes
	log.Printf("Starting ticker with %d minute intervals", MINUTES_BETWEEN_CHECKS)
	t := time.NewTicker(MINUTES_BETWEEN_CHECKS * time.Minute)
	for {
		currentTime := <-t.C
		log.Printf("Checking for events since %s\n", previousTime.Format(time.RFC3339))
		processEvents()
		previousTime = currentTime
	}
}

// Generic GraphQL request function
func makeGraphQLRequest[T any](url, query string) (*T, error) {
	reqBodyBytes, err := json.Marshal(GraphQLRequest{Query: query})
	if err != nil {
		return nil, fmt.Errorf("error marshalling request: %w", err)
	}
	
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(reqBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("error posting request: %w", err)
	}
	defer resp.Body.Close()
	
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response: %w", err)
	}

	var result T
	if err = json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("error unmarshalling response: %w", err)
	}
	return &result, nil
}

func v3PayEvents(since time.Time, subgraphUrl string) (*V3PayEventsResponse, error) {
	query := fmt.Sprintf(`{
		payEvents(
		  first: 1000
		  orderBy: timestamp
		  orderDirection: asc
		  where: {timestamp_gt:%d}) {
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
	}`, since.Unix())
	return makeGraphQLRequest[V3PayEventsResponse](subgraphUrl, query)
}

func v3NewProjects(since time.Time, subgraphUrl string) (*V3ProjectsResponse, error) {
	query := fmt.Sprintf(`{
		projects(
		  first: 1000
		  orderBy: createdAt
		  orderDirection: desc
		  where: {createdAt_gt:%d}) {
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
	}`, since.Unix())
	return makeGraphQLRequest[V3ProjectsResponse](subgraphUrl, query)
}

func v4PayEvents(since time.Time, bendystrawUrl string) (*V4PayEventsResponse, error) {
	query := fmt.Sprintf(`{
		payEvents(
		  orderBy: "timestamp"
		  orderDirection: "asc"
		  where: {timestamp_gt:%d}
		  limit: 1000) {
			items {
				chainId
				projectId
				amount
				amountUsd
				timestamp
				beneficiary
				txHash
				memo
				project {
					handle
					metadataUri
					creator
					owner
					isRevnet
				}
			}
		}
	}`, since.Unix())
	return makeGraphQLRequest[V4PayEventsResponse](bendystrawUrl, query)
}

func v4NewProjects(since time.Time, bendystrawUrl string) (*V4ProjectsResponse, error) {
	query := fmt.Sprintf(`{
		projects(
		  orderBy: "createdAt"
		  orderDirection: "desc"
		  where: {createdAt_gt:%d}
		  limit: 1000) {
			items {
				chainId
				projectId
				handle
				metadataUri
				creator
				owner
				isRevnet
			}
		}
	}`, since.Unix())
	return makeGraphQLRequest[V4ProjectsResponse](bendystrawUrl, query)
}

// Check if a pay event matches a config string
func matchesPayEventConfig(configKey string, projectId int, chainId int, version string, isRevnet bool) bool {
	// Handle global wildcard - all payments across all versions
	if configKey == "pay" {
		return true
	}
	
	// Handle revnet payments - only v4 projects with isRevnet = true
	if configKey == "revnet" {
		return version == "4" && isRevnet
	}
	
	// Handle v4 format: chainId:projectId
	if strings.Contains(configKey, ":") {
		parts := strings.Split(configKey, ":")
		if len(parts) == 2 {
			if configChainId, err := strconv.Atoi(parts[0]); err == nil {
				if configProjectId, err := strconv.Atoi(parts[1]); err == nil {
					return version == "4" && chainId == configChainId && projectId == configProjectId
				}
			}
		}
	} else {
		// Handle v2/v3 format: just projectId
		if configProjectId, err := strconv.Atoi(configKey); err == nil {
			return (version == "2" || version == "3") && chainId == 1 && projectId == configProjectId
		}
	}
	
	return false
}

// Group projects by metadata URI and creator (cross-chain deployments)
func groupCrossChainV4Projects(projects []ProjectV4) [][]ProjectV4 {
	groups := make(map[string][]ProjectV4)
	
	for _, project := range projects {
		// Use metadataUri + creator as the key to group cross-chain projects
		key := project.MetadataUri + "|" + project.Creator
		groups[key] = append(groups[key], project)
	}
	
	var result [][]ProjectV4
	for _, group := range groups {
		result = append(result, group)
	}
	
	return result
}

// Process v3 pay event and send alerts to any matching channels
func processV3PayEvent(event PayEvent, config map[string][]string, metadataCache *MetadataCache, s *discordgo.Session) {
	cacheKey := fmt.Sprintf("%s:1:%d", event.Pv, event.ProjectId)
	metadata := memoizedMetadata(metadataCache, cacheKey, event.Project.MetadataUri, event.Project.Handle, event.Pv)

	for channel, configKeys := range config {
		for _, configKey := range configKeys {
			if matchesPayEventConfig(configKey, event.ProjectId, 1, event.Pv, false) {
				go func(channelID string) {
					_, err := s.ChannelMessageSendEmbed(channelID, formatV3PayEvent(event, metadata))
					if err != nil {
						log.Printf("Failed to send message to channel %s: %v\n", channelID, err)
					}
				}(channel)
				break // Only send once per channel
			}
		}
	}
}

// Process v4 pay event and send alerts to any matching channels
func processV4PayEvent(event PayEventV4, config map[string][]string, metadataCache *MetadataCache, s *discordgo.Session) {
	cacheKey := fmt.Sprintf("v4:%d:%d", event.ChainId, event.ProjectId)
	
	var metadataUri, handle string
	var isRevnet bool
	if event.Project != nil {
		metadataUri = event.Project.MetadataUri
		handle = event.Project.Handle
		isRevnet = event.Project.IsRevnet
	}
	
	metadata := memoizedMetadata(metadataCache, cacheKey, metadataUri, handle, "4")

	for channel, configKeys := range config {
		for _, configKey := range configKeys {
			if matchesPayEventConfig(configKey, event.ProjectId, event.ChainId, "4", isRevnet) {
				go func(channelID string) {
					_, err := s.ChannelMessageSendEmbed(channelID, formatV4PayEvent(event, metadata))
					if err != nil {
						log.Printf("Failed to send message to channel %s: %v\n", channelID, err)
					}
				}(channel)
				break // Only send once per channel
			}
		}
	}
}

// Process a new v3 project event and send alerts to any matching channels
func processV3Project(event Project, config map[string][]string, metadataCache *MetadataCache, s *discordgo.Session) {
	cacheKey := fmt.Sprintf("%s:1:%d", event.Pv, event.ProjectId)
	metadata := memoizedMetadata(metadataCache, cacheKey, event.MetadataUri, event.Handle, event.Pv)

	for channel, configKeys := range config {
		for _, configKey := range configKeys {
			if configKey == "new" {
				go func(channelID string) {
					_, err := s.ChannelMessageSendEmbed(channelID, formatV3Project(event, metadata))
					if err != nil {
						log.Printf("Failed to send message to channel %s: %v\n", channelID, err)
					}
				}(channel)
				break // Only send once per channel
			}
		}
	}
}

// Process a group of v4 projects (potentially cross-chain) and send alerts to any matching channels
func processV4ProjectGroup(projectGroup []ProjectV4, config map[string][]string, metadataCache *MetadataCache, s *discordgo.Session) {
	if len(projectGroup) == 0 {
		return
	}
	
	// Use the first project for basic info
	firstProject := projectGroup[0]
	
	cacheKey := fmt.Sprintf("v4:group:%s:%s", firstProject.MetadataUri, firstProject.Creator)
	metadata := memoizedMetadata(metadataCache, cacheKey, firstProject.MetadataUri, firstProject.Handle, "4")

	for channel, configKeys := range config {
		for _, configKey := range configKeys {
			if configKey == "new" || (configKey == "revnet" && firstProject.IsRevnet) {
				go func(channelID string) {
					_, err := s.ChannelMessageSendEmbed(channelID, formatV4ProjectGroup(projectGroup, metadata))
					if err != nil {
						log.Printf("Failed to send message to channel %s: %v\n", channelID, err)
					}
				}(channel)
				break // Only send once per channel
			}
		}
	}
}

func formatV3PayEvent(event PayEvent, m Metadata) *discordgo.MessageEmbed {
	fields := make([]*discordgo.MessageEmbedField, 0, 6)
	
	// Check for IPFS image in note/memo first
	var noteImage string
	noteText := event.Note
	if event.Note != "" {
		if imageUrl := extractIPFSImage(event.Note); imageUrl != "" {
			noteImage = imageUrl
		}
		// Remove IPFS URLs from the note text
		noteText = removeIPFSUrls(noteText)
		
		if noteText != "" {
			fields = append(fields, &discordgo.MessageEmbedField{
				Name:   "Note",
				Value:  noteText,
				Inline: false,
			})
		}
	}

	// Field order: Amount, Beneficiary, Transaction
	
	// 1. Amount
	if event.Amount != "" {
		amountStr, err := parseFixedPointString(event.Amount, 18, -1)
		if err == nil {
			value := fmt.Sprintf("%s ETH", amountStr)
			if event.AmountUSD != "" {
				amountUsdStr, usdErr := parseFixedPointString(event.AmountUSD, 18, 2)
				if usdErr == nil {
					value = fmt.Sprintf("%s ETH ($%s USD)", amountStr, amountUsdStr)
				}
			}
			fields = append(fields, &discordgo.MessageEmbedField{
				Name:   "Amount",
				Value:  value,
				Inline: true,
			})
		}
	}

	// 2. Beneficiary
	if event.Beneficiary != "" {
		beneficiary, beneficiaryUrl := formatAddressLink(event.Beneficiary, 1)

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Beneficiary",
			Value:  fmt.Sprintf("[%s](%s)", beneficiary, beneficiaryUrl),
			Inline: true,
		})
	}

	// 3. Transaction
	if event.TxHash != "" {
		explorerUrl := getExplorerUrl(1, fmt.Sprintf("tx/%s", event.TxHash))
		
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Transaction",
			Value:  fmt.Sprintf("[Explorer](%s)", explorerUrl),
			Inline: true,
		})
	}

	projectLink := getProjectLink(event.Pv, 1, event.ProjectId, event.Project.Handle)
	
	title := fmt.Sprintf("Payment to %s", m.Name)

	embed := &discordgo.MessageEmbed{
		Title:     title,
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: getUrlFromUri(m.LogoUri)},
		URL:       projectLink,
		Color:     getProjectColor(event.Pv, 1, event.ProjectId),
		Fields:    fields,
	}
	
	// Add image if found in note
	if noteImage != "" {
		// Put note image in main image field (thumbnail is used for project logo)
		embed.Image = &discordgo.MessageEmbedImage{URL: noteImage}
	}
	
	return embed
}

func formatV4PayEvent(event PayEventV4, m Metadata) *discordgo.MessageEmbed {
	fields := make([]*discordgo.MessageEmbedField, 0, 6)
	
	// Check for IPFS image in memo first
	var noteImage string
	noteText := event.Memo
	if event.Memo != "" {
		if imageUrl := extractIPFSImage(event.Memo); imageUrl != "" {
			noteImage = imageUrl
		}
		// Remove ALL IPFS URLs from the note text
		noteText = removeIPFSUrls(noteText)
		
		if noteText != "" {
			fields = append(fields, &discordgo.MessageEmbedField{
				Name:   "Note",
				Value:  noteText,
				Inline: false,
			})
		}
	}

	// Field order: Amount, Network, Beneficiary, Transaction
	
	// 1. Amount
	if event.Amount != "" {
		amountStr, err := parseFixedPointString(event.Amount, 18, -1)
		if err == nil {
			value := fmt.Sprintf("%s ETH", amountStr)
			if event.AmountUsd != "" {
				amountUsdStr, usdErr := parseFixedPointString(event.AmountUsd, 18, 2)
				if usdErr == nil {
					value = fmt.Sprintf("%s ETH ($%s USD)", amountStr, amountUsdStr)
				}
			}
			fields = append(fields, &discordgo.MessageEmbedField{
				Name:   "Amount",
				Value:  value,
				Inline: true,
			})
		}
	}

	// 2. Network
	chainName := getChainName(event.ChainId)
	fields = append(fields, &discordgo.MessageEmbedField{
		Name:   "Network",
		Value:  chainName,
		Inline: true,
	})

	// 3. Beneficiary
	if event.Beneficiary != "" {
		beneficiary, beneficiaryUrl := formatAddressLink(event.Beneficiary, event.ChainId)

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Beneficiary",
			Value:  fmt.Sprintf("[%s](%s)", beneficiary, beneficiaryUrl),
			Inline: true,
		})
	}

	// 4. Transaction
	if event.TxHash != "" {
		explorerUrl := getExplorerUrl(event.ChainId, fmt.Sprintf("tx/%s", event.TxHash))
		
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Transaction",
			Value:  fmt.Sprintf("[Explorer](%s)", explorerUrl),
			Inline: true,
		})
	}

	handle := ""
	if event.Project != nil {
		handle = event.Project.Handle
	}
	projectLink := getProjectLink("4", event.ChainId, event.ProjectId, handle)
	
	title := fmt.Sprintf("Payment to %s", m.Name)

	embed := &discordgo.MessageEmbed{
		Title:     title,
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: getUrlFromUri(m.LogoUri)},
		URL:       projectLink,
		Color:     getProjectColor("4", event.ChainId, event.ProjectId),
		Fields:    fields,
	}
	
	// Add image if found in note
	if noteImage != "" {
		embed.Image = &discordgo.MessageEmbedImage{URL: noteImage}
	}
	
	return embed
}

func formatV3Project(event Project, m Metadata) *discordgo.MessageEmbed {
	fields := make([]*discordgo.MessageEmbedField, 0, 5)

	if event.Creator != "" {
		creator, creatorUrl := formatAddressLink(event.Creator, 1)

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Creator",
			Value:  fmt.Sprintf("[%s](%s)", creator, creatorUrl),
			Inline: true,
		})
	}

	if event.Creator != event.Owner && event.Owner != "" {
		owner, ownerUrl := formatAddressLink(event.Owner, 1)

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Owner",
			Value:  fmt.Sprintf("[%s](%s)", owner, ownerUrl),
			Inline: true,
		})
	}

	if len(event.InitEvents) > 0 && event.InitEvents[0].TxHash != "" {
		explorerUrl := getExplorerUrl(1, fmt.Sprintf("tx/%s", event.InitEvents[0].TxHash))
		
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Transaction",
			Value:  fmt.Sprintf("[Explorer](%s)", explorerUrl),
			Inline: true,
		})
	}

	projectLink := getProjectLink(event.Pv, 1, event.ProjectId, event.Handle)
	title := fmt.Sprintf("New project: %s", m.Name)
	
	// Put tagline directly as description (no label)
	var description string
	if m.ProjectTagline != "" {
		description = m.ProjectTagline
	}

	return &discordgo.MessageEmbed{
		Title:       title,
		Description: description,
		Thumbnail:   &discordgo.MessageEmbedThumbnail{URL: getUrlFromUri(m.LogoUri)},
		URL:         projectLink,
		Color:       getProjectColor(event.Pv, 1, event.ProjectId),
		Fields:      fields,
	}
}

func formatV4ProjectGroup(projectGroup []ProjectV4, m Metadata) *discordgo.MessageEmbed {
	if len(projectGroup) == 0 {
		return nil
	}
	
	// Use first project for basic info
	firstProject := projectGroup[0]
	
	fields := make([]*discordgo.MessageEmbedField, 0, 5)

	// Build networks list
	var networks []string
	var projectLinks []string
	for _, project := range projectGroup {
		chainName := getChainName(project.ChainId)
		networks = append(networks, chainName)
		
		// Add project links for each network
		link := getProjectLink("4", project.ChainId, project.ProjectId, project.Handle)
		projectLinks = append(projectLinks, fmt.Sprintf("[%s](%s)", chainName, link))
	}
	
	// For single network, show as "Network", for multiple show as "Links"
	if len(networks) == 1 {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Network",
			Value:  networks[0],
			Inline: true,
		})
	}

	if firstProject.Creator != "" {
		creator, creatorUrl := formatAddressLink(firstProject.Creator, firstProject.ChainId)

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Creator",
			Value:  fmt.Sprintf("[%s](%s)", creator, creatorUrl),
			Inline: true,
		})
	}

	if firstProject.Creator != firstProject.Owner && firstProject.Owner != "" {
		owner, ownerUrl := formatAddressLink(firstProject.Owner, firstProject.ChainId)

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Owner",
			Value:  fmt.Sprintf("[%s](%s)", owner, ownerUrl),
			Inline: true,
		})
	}
	
	// If multiple networks, add links field instead of networks
	if len(projectLinks) > 1 {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Links",
			Value:  strings.Join(projectLinks, " • "),
			Inline: false,
		})
	}

	title := fmt.Sprintf("New project: %s", m.Name)
	if firstProject.IsRevnet {
		title = fmt.Sprintf("New revnet: %s", m.Name)
	}

	// Put tagline directly as description (no label)
	var description string
	if m.ProjectTagline != "" {
		description = m.ProjectTagline
	}

	// Use the first project's link as the main URL
	mainProjectLink := getProjectLink("4", firstProject.ChainId, firstProject.ProjectId, firstProject.Handle)

	return &discordgo.MessageEmbed{
		Title:       title,
		Description: description,
		Thumbnail:   &discordgo.MessageEmbedThumbnail{URL: getUrlFromUri(m.LogoUri)},
		URL:         mainProjectLink,
		Color:       getProjectColor("4", firstProject.ChainId, firstProject.ProjectId),
		Fields:      fields,
	}
}

func getProjectLink(version string, chainId int, projectId int, handle string) string {
	// v1 projects use handle
	if version == "1" {
		return fmt.Sprintf("https://juicebox.money/p/%s", handle)
	}
	
	// v2 and v3 projects use v2/p/ format
	if version == "2" || version == "3" {
		return fmt.Sprintf("https://juicebox.money/v2/p/%d", projectId)
	}
	
	// v4 projects use v4/{network}:{projectId} format
	if version == "4" {
		return fmt.Sprintf("https://juicebox.money/v4/%s:%d", getChainShortName(chainId), projectId)
	}
	
	// Fallback
	log.Printf("Unknown version: %s for project %d\n", version, projectId)
	return fmt.Sprintf("https://juicebox.money/p/%d", projectId)
}

type ChainInfo struct {
	Name        string
	ShortName   string
	ExplorerUrl string
}

var chains = map[int]ChainInfo{
	1:     {"Ethereum", "eth", "https://etherscan.io"},
	10:    {"Optimism", "op", "https://optimistic.etherscan.io"},
	8453:  {"Base", "base", "https://basescan.org"},
	42161: {"Arbitrum", "arb", "https://arbiscan.io"},
}

func getChainName(chainId int) string {
	if chain, ok := chains[chainId]; ok {
		return chain.Name
	}
	return fmt.Sprintf("Chain %d", chainId)
}

func getChainShortName(chainId int) string {
	if chain, ok := chains[chainId]; ok {
		return chain.ShortName
	}
	return fmt.Sprintf("chain%d", chainId)
}

func getExplorerUrl(chainId int, path string) string {
	explorer := "https://etherscan.io" // fallback
	if chain, ok := chains[chainId]; ok {
		explorer = chain.ExplorerUrl
	}
	return fmt.Sprintf("%s/%s", explorer, path)
}

// Format address as a link with ENS resolution
func formatAddressLink(address string, chainId int) (string, string) {
	// Get display name with combined ENS/truncation logic
	var displayName string
	
	// Make sure address is valid before checking ENS
	if len(address) == 42 && address[0:2] == "0x" {
		// Try ENS resolution
		resp, err := http.Get("https://api.ensideas.com/ens/resolve/" + address)
		if err == nil {
			defer resp.Body.Close()
			respBody, err := io.ReadAll(resp.Body)
			if err == nil {
				var ensResp struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(respBody, &ensResp) == nil && ensResp.Name != "" {
					displayName = ensResp.Name
				}
			}
		}
	}
	
	// Fallback to truncated address if no ENS name
	if displayName == "" {
		if len(address) < 10 {
			displayName = address
		} else {
			displayName = fmt.Sprintf("%s...%s", address[:6], address[len(address)-4:])
		}
	}
	
	return displayName, getExplorerUrl(chainId, fmt.Sprintf("address/%s", address))
}

// IPFS URL patterns used by both extract and remove functions
var ipfsPatterns = []string{
	`https://[^\s]*\.infura-ipfs\.io/ipfs/[^\s]+`,
	`https://[^\s]*/ipfs/[^\s]+`,
	`ipfs://[^\s]+`,
}

// Extract first IPFS image URL from text
func extractIPFSImage(text string) string {
	for _, pattern := range ipfsPatterns {
		if match := regexp.MustCompile(pattern).FindString(text); match != "" {
			return strings.TrimSpace(match)
		}
	}
	return ""
}

// Remove all IPFS URLs from text
func removeIPFSUrls(text string) string {
	result := text
	for _, pattern := range ipfsPatterns {
		result = regexp.MustCompile(pattern).ReplaceAllString(result, "")
	}
	return strings.TrimSpace(regexp.MustCompile(`\s+`).ReplaceAllString(result, " "))
}

// Generate deterministic color based on project identity
func getProjectColor(version string, chainId int, projectId int) int {
	// Create a hash input from version, chainId, and projectId
	input := fmt.Sprintf("%s:%d:%d", version, chainId, projectId)
	hash := sha256.Sum256([]byte(input))
	
	// Use first 3 bytes of hash for RGB values
	r := int(hash[0])
	g := int(hash[1]) 
	b := int(hash[2])
	
	// Convert to Discord color (24-bit integer)
	color := (r << 16) | (g << 8) | b
	
	// Ensure it's not too dark by adding minimum brightness
	if r+g+b < 180 {
		// Brighten the color by adding to each component
		brightR := min(255, r+60)
		brightG := min(255, g+60)
		brightB := min(255, b+60)
		color = (brightR << 16) | (brightG << 8) | brightB
	}
	
	return color
}


// Parse fixed point string with optional precision. If precision is -1, uses smart formatting
func parseFixedPointString(s string, decimals int64, precision int) (string, error) {
	bf, ok := new(big.Float).SetString(s)
	if !ok {
		return "", fmt.Errorf("error parsing fixed decimal string: %s", s)
	}

	divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(decimals), nil)
	floatDivisor := new(big.Float).SetInt(divisor)
	result := new(big.Float).Quo(bf, floatDivisor)

	// Smart formatting (precision -1) - shows up to 5 decimals but doesn't pad with zeros
	if precision == -1 {
		// Check if the original value is exactly zero
		if bf.Sign() == 0 {
			return "0", nil
		}

		// Convert to string with max 5 decimals, then trim trailing zeros
		formatted := result.Text('f', 5)
		
		// Handle very small amounts that round to 0 (but aren't actually 0)
		if formatted == "0.00000" {
			return "~0", nil
		}
		
		// Remove trailing zeros and decimal point if not needed
		formatted = strings.TrimRight(formatted, "0")
		formatted = strings.TrimRight(formatted, ".")
		
		return formatted, nil
	}

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
			} else {
				cacheValue.Metadata = *newMetadata
			}
		}

		close(cacheValue.ready)
	} else {
		m.Unlock()
		<-cacheValue.ready
	}

	return cacheValue.Metadata
}

func createPlaceholderCacheValue(projectId string, handle string, pv string) *MetadataCacheValue {
	metadata := &Metadata{Name: fmt.Sprintf("Project %s", projectId)}
	if pv == "4" {
		metadata.Name = fmt.Sprintf("Project %s (v4)", projectId)
	} else if pv == "3" || pv == "2" {
		metadata.InfoUri = fmt.Sprintf("https://juicebox.money/v2/p/%s", projectId)
	} else if pv == "1" {
		metadata.Name += " (v1)"
		metadata.InfoUri = fmt.Sprintf("https://juicebox.money/p/%s", handle)
	}

	return &MetadataCacheValue{
		MetadataIPFSUri: "",
		Metadata:        *metadata,
		ready:           make(chan struct{}),
	}
}
