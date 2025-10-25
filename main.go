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
	testingLookbackDays    = 4
)

func main() {
	if err := godotenv.Load(".env"); err != nil {
		log.Fatalln("No .env file found")
	}
	discordToken := os.Getenv("DISCORD_TOKEN")
	if discordToken == "" {
		log.Fatalln("Could not find DISCORD_TOKEN in .env file")
	}

	// The subgraph provides events for Juicebox v1, v2, and v3.
	subgraphURL := os.Getenv("SUBGRAPH_URL")
	if subgraphURL == "" {
		log.Fatalln("Could not find SUBGRAPH_URL in .env file")
	}
	// Bendystraw provides events for Juicebox v4 and v5.
	bendystrawURL := os.Getenv("BENDYSTRAW_URL")
	if bendystrawURL == "" {
		log.Fatalln("Could not find BENDYSTRAW_URL in .env file")
	}

	// When testing, we log events from the past N days then exit
	testing := os.Getenv("TESTING") == "1"
	configPath := "config.json"
	if testing {
		configPath = "test_config.json"
	}

	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatalf("Failed to read config file %s: %v\n", configPath, err)
	}
	// config is a map of channel IDs to notification types - see README.md
	var config map[string][]string
	err = json.Unmarshal(configBytes, &config)
	if err != nil {
		log.Fatalf("Failed to parse config file %s: %v\n", configPath, err)
	}

	alerts, err := buildAlertsConfig(config)
	if err != nil {
		log.Fatalf("Invalid config: %v\n", err)
	}

	// The metadata cache is a map of project IDs to MetadataCacheValues.
	metadataCache := MetadataCache{
		Map: make(map[string]*MetadataCacheValue),
	}

	discordSession, err := discordgo.New("Bot " + discordToken)
	if err != nil {
		log.Fatalf("Failed to create discord session: %v\n", err)
	}
	discordSession.ShouldRetryOnRateLimit = true
	discordSession.ShouldReconnectOnError = true
	discordSession.AddHandler(func(s *discordgo.Session, d *discordgo.Disconnect) {
		log.Println("Discord disconnected, reconnecting...")
	})
	if err = discordSession.Open(); err != nil {
		log.Fatalf("Failed to open discord session: %v\n", err)
	}
	defer discordSession.Close()
	log.Println("Discord session opened")

	previousTime := time.Now()
	if testing {
		previousTime = time.Now().AddDate(0, 0, -testingLookbackDays)
		log.Printf("Testing mode: checking events from the past %d days (since %v)", testingLookbackDays, previousTime)
	}

	processOnce := func(since time.Time) {
		payResp, err := v3PayEvents(since, subgraphURL)
		if err != nil {
			log.Printf("Failed to get v3 pay events: %v\n", err)
		}
		projResp, err := v3NewProjects(since, subgraphURL)
		if err != nil {
			log.Printf("Failed to get new v3 projects: %v\n", err)
		}

		payRespBendy, err := bendyPayEvents(since, bendystrawURL)
		if err != nil {
			log.Printf("Failed to get bendystraw pay events: %v\n", err)
		}
		projRespBendy, err := bendyProjects(since, bendystrawURL)
		if err != nil {
			log.Printf("Failed to get new bendystraw projects: %v\n", err)
		}

		if payResp != nil && len(payResp.Data.PayEvents) > 0 {
			log.Printf("Found %d v3 pay events", len(payResp.Data.PayEvents))
			for _, payEvent := range payResp.Data.PayEvents {
				processV3PayEvent(payEvent, alerts, &metadataCache, discordSession)
			}
		}
		if projResp != nil && len(projResp.Data.Projects) > 0 {
			log.Printf("Found %d new v3 projects", len(projResp.Data.Projects))
			for _, newProject := range projResp.Data.Projects {
				processV3Project(newProject, alerts, &metadataCache, discordSession)
			}
		}

		if payRespBendy != nil && len(payRespBendy.Data.PayEvents.Items) > 0 {
			log.Printf("Found %d bendystraw pay events", len(payRespBendy.Data.PayEvents.Items))
			for _, payEvent := range payRespBendy.Data.PayEvents.Items {
				processBendyPayEvent(payEvent, alerts, &metadataCache, discordSession)
			}
		}
		if projRespBendy != nil && len(projRespBendy.Data.Projects.Items) > 0 {
			log.Printf("Found %d new bendystraw projects", len(projRespBendy.Data.Projects.Items))
			for _, projectGroup := range groupCrossChainProjects(projRespBendy.Data.Projects.Items) {
				processBendyProjectGroup(projectGroup, alerts, &metadataCache, discordSession)
			}
		}
	}

	if testing {
		// In testing mode, run once immediately and exit
		log.Println("Running event processing once in testing mode...")
		processOnce(previousTime)
		return
	}

	// Blocking loop to check for events every MINUTES_BETWEEN_CHECKS minutes
	log.Printf("Starting ticker with %d minute intervals", MINUTES_BETWEEN_CHECKS)
	t := time.NewTicker(MINUTES_BETWEEN_CHECKS * time.Minute)
	for {
		currentTime := <-t.C
		log.Printf("Checking for events since %s\n", previousTime.Format(time.RFC3339))
		processOnce(previousTime)
		previousTime = currentTime
	}
}

type alertsConfig struct {
	pay      []string
	new      []string
	revnet   []string
	projects map[projectKey][]string
}

type projectKey struct {
	version string
	chain   int
	project int
}

func buildAlertsConfig(raw map[string][]string) (*alertsConfig, error) {
	cfg := &alertsConfig{projects: make(map[projectKey][]string)}
	for channel, rules := range raw {
		if channel == "" {
			return nil, fmt.Errorf("empty channel id in config")
		}
		for _, rule := range rules {
			normalized := strings.ToLower(strings.TrimSpace(rule))
			if normalized == "" {
				return nil, fmt.Errorf("empty rule for channel %s", channel)
			}
			switch normalized {
			case "pay":
				cfg.pay = append(cfg.pay, channel)
			case "new":
				cfg.new = append(cfg.new, channel)
			case "revnet":
				cfg.revnet = append(cfg.revnet, channel)
			default:
				parts := strings.Split(normalized, ":")
				switch parts[0] {
				case "v3":
					if len(parts) != 2 {
						return nil, fmt.Errorf("invalid v3 rule %q for channel %s", rule, channel)
					}
					projectID, err := strconv.Atoi(parts[1])
					if err != nil {
						return nil, fmt.Errorf("invalid project id in rule %q: %w", rule, err)
					}
					key := projectKey{version: "v3", chain: 1, project: projectID}
					cfg.projects[key] = append(cfg.projects[key], channel)
				case "v4", "v5":
					if len(parts) != 3 {
						return nil, fmt.Errorf("invalid %s rule %q for channel %s", parts[0], rule, channel)
					}
					chainID, errChain := strconv.Atoi(parts[1])
					projectID, errProj := strconv.Atoi(parts[2])
					if errChain != nil || errProj != nil {
						return nil, fmt.Errorf("invalid chain/project in rule %q", rule)
					}
					key := projectKey{version: parts[0], chain: chainID, project: projectID}
					cfg.projects[key] = append(cfg.projects[key], channel)
				default:
					return nil, fmt.Errorf("unknown rule %q for channel %s", rule, channel)
				}
			}
		}
	}
	return cfg, nil
}

func sendToChannels(session *discordgo.Session, embed *discordgo.MessageEmbed, lists ...[]string) {
	if embed == nil {
		return
	}
	seen := make(map[string]struct{})
	for _, list := range lists {
		for _, channel := range list {
			if channel == "" {
				continue
			}
			if _, ok := seen[channel]; ok {
				continue
			}
			if _, err := session.ChannelMessageSendEmbed(channel, embed); err != nil {
				log.Printf("Failed to send message to channel %s: %v\n", channel, err)
			}
			seen[channel] = struct{}{}
		}
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

func v3PayEvents(since time.Time, subgraphURL string) (*V3PayEventsResponse, error) {
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
	return makeGraphQLRequest[V3PayEventsResponse](subgraphURL, query)
}

func v3NewProjects(since time.Time, subgraphURL string) (*V3ProjectsResponse, error) {
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
	return makeGraphQLRequest[V3ProjectsResponse](subgraphURL, query)
}

func bendyPayEvents(since time.Time, bendystrawURL string) (*BendyPayEventsResponse, error) {
	query := fmt.Sprintf(`{
		payEvents(
		  orderBy: "timestamp"
		  orderDirection: "asc"
		  where: {timestamp_gt:%d}
		  limit: 1000) {
			items {
				chainId
				projectId
				version
				amount
				amountUsd
				timestamp
				beneficiary
				txHash
				memo
				caller
				from
				project {
					handle
					metadataUri
					creator
					owner
					isRevnet
					version
					suckerGroupId
					token
					tokenSymbol
					decimals
				}
			}
		}
	}`, since.Unix())
	return makeGraphQLRequest[BendyPayEventsResponse](bendystrawURL, query)
}

func bendyProjects(since time.Time, bendystrawURL string) (*BendyProjectsResponse, error) {
	query := fmt.Sprintf(`{
		projects(
		  orderBy: "createdAt"
		  orderDirection: "desc"
		  where: {createdAt_gt:%d}
		  limit: 1000) {
			items {
				chainId
				projectId
				version
				handle
				metadataUri
				creator
				owner
				isRevnet
				suckerGroupId
				projectCreateEvents(
				  orderBy: "timestamp"
				  orderDirection: "desc"
				  limit: 1
				) {
					items {
						txHash
					}
				}
			}
		}
	}`, since.Unix())
	return makeGraphQLRequest[BendyProjectsResponse](bendystrawURL, query)
}

// Group projects by suckerGroupID when present, otherwise fall back to metadata + creator + version
func groupCrossChainProjects(projects []BendyProject) [][]BendyProject {
	groups := make(map[string][]BendyProject)

	for _, project := range projects {
		key := project.SuckerGroupID
		if key != "" {
			key = fmt.Sprintf("%s|v%d", key, project.Version)
		} else {
			key = fmt.Sprintf("v%d|%s|%s|%s", project.Version, project.MetadataURI, project.Creator, project.Handle)
		}
		groups[key] = append(groups[key], project)
	}

	var result [][]BendyProject
	for _, group := range groups {
		result = append(result, group)
	}

	return result
}

// Process v3 pay event and send alerts to any matching channels
func processV3PayEvent(event PayEvent, cfg *alertsConfig, metadataCache *MetadataCache, session *discordgo.Session) {
	cacheKey := fmt.Sprintf("%s:1:%d", event.Pv, event.ProjectID)
	metadata := memoizedMetadata(metadataCache, cacheKey, event.Project.MetadataURI, event.Project.Handle, event.Pv)

	opts := payEmbedOptions{
		memo:           event.Note,
		amount:         event.Amount,
		amountUSD:      event.AmountUSD,
		beneficiary:    event.Beneficiary,
		chainID:        1,
		txHash:         event.TxHash,
		projectVersion: event.Pv,
		projectChain:   1,
		projectID:      event.ProjectID,
		handle:         event.Project.Handle,
		tokenSymbol:    getNativeTokenSymbol(1),
		tokenDecimals:  18,
		showUSD:        true,
	}
	embed := buildPayEmbed(metadata, opts)
	key := projectKey{version: "v3", chain: 1, project: event.ProjectID}
	sendToChannels(session, embed, cfg.pay, cfg.projects[key])
}

// Process v4 pay event and send alerts to any matching channels
func processBendyPayEvent(event BendyPayEvent, cfg *alertsConfig, metadataCache *MetadataCache, session *discordgo.Session) {
	versionStr := strconv.Itoa(event.Version)
	cacheKey := fmt.Sprintf("v%s:%d:%d", versionStr, event.ChainID, event.ProjectID)

	var metadataURI, handle string
	var isRevnet bool
	tokenSymbol := ""
	tokenDecimals := 18
	if event.Project != nil {
		metadataURI = event.Project.MetadataURI
		handle = event.Project.Handle
		isRevnet = event.Project.IsRevnet
		if event.Project.Version != 0 {
			versionStr = strconv.Itoa(event.Project.Version)
		}
		if event.Project.TokenSymbol != nil {
			tokenSymbol = *event.Project.TokenSymbol
		}
		if event.Project.Decimals != nil && *event.Project.Decimals > 0 {
			tokenDecimals = *event.Project.Decimals
		}
	}

	metadata := memoizedMetadata(metadataCache, cacheKey, metadataURI, handle, versionStr)

	versionLabel := fmt.Sprintf("v%s", versionStr)
	nativeSymbol := getNativeTokenSymbol(event.ChainID)
	if tokenDecimals <= 0 {
		tokenDecimals = 18
	}
	if tokenSymbol == "" {
		tokenSymbol = nativeSymbol
	}
	showUSD := event.AmountUSD != "" && event.AmountUSD != "0"
	opts := payEmbedOptions{
		memo:           event.Memo,
		amount:         event.Amount,
		amountUSD:      event.AmountUSD,
		beneficiary:    event.Beneficiary,
		chainID:        event.ChainID,
		txHash:         event.TxHash,
		projectVersion: versionStr,
		projectChain:   event.ChainID,
		projectID:      event.ProjectID,
		handle:         handle,
		networkName:    getChainName(event.ChainID),
		tokenSymbol:    tokenSymbol,
		tokenDecimals:  tokenDecimals,
		showUSD:        showUSD,
	}
	embed := buildPayEmbed(metadata, opts)
	key := projectKey{version: versionLabel, chain: event.ChainID, project: event.ProjectID}
	if isRevnet {
		sendToChannels(session, embed, cfg.pay, cfg.revnet, cfg.projects[key])
	} else {
		sendToChannels(session, embed, cfg.pay, cfg.projects[key])
	}
}

// Process a new v3 project event and send alerts to any matching channels
func processV3Project(event Project, cfg *alertsConfig, metadataCache *MetadataCache, session *discordgo.Session) {
	cacheKey := fmt.Sprintf("%s:1:%d", event.Pv, event.ProjectID)
	metadata := memoizedMetadata(metadataCache, cacheKey, event.MetadataURI, event.Handle, event.Pv)

	embed := formatV3Project(event, metadata)
	sendToChannels(session, embed, cfg.new)
}

// Process a group of v4 projects (potentially cross-chain) and send alerts to any matching channels
func processBendyProjectGroup(projectGroup []BendyProject, cfg *alertsConfig, metadataCache *MetadataCache, session *discordgo.Session) {
	if len(projectGroup) == 0 {
		return
	}

	// Use the first project for basic info
	firstProject := projectGroup[0]
	versionStr := strconv.Itoa(firstProject.Version)

	cacheKey := fmt.Sprintf("v%s:group:%s:%s:%s", versionStr, firstProject.SuckerGroupID, firstProject.MetadataURI, firstProject.Creator)
	metadata := memoizedMetadata(metadataCache, cacheKey, firstProject.MetadataURI, firstProject.Handle, versionStr)

	embed := formatBendyProjectGroup(projectGroup, metadata)
	if firstProject.IsRevnet {
		sendToChannels(session, embed, cfg.new, cfg.revnet)
	} else {
		sendToChannels(session, embed, cfg.new)
	}
}

type payEmbedOptions struct {
	memo           string
	amount         string
	amountUSD      string
	beneficiary    string
	chainID        int
	txHash         string
	projectVersion string
	projectChain   int
	projectID      int
	handle         string
	networkName    string
	tokenSymbol    string
	tokenDecimals  int
	showUSD        bool
}

func buildPayEmbed(m Metadata, opts payEmbedOptions) *discordgo.MessageEmbed {
	fields := make([]*discordgo.MessageEmbedField, 0, 6)

	memo := strings.TrimSpace(opts.memo)
	noteImage := ""
	if memo != "" {
		noteImage = extractIPFSImage(memo)
		cleaned := removeIPFSURLs(memo)
		if cleaned != "" {
			fields = append(fields, &discordgo.MessageEmbedField{Name: "Note", Value: cleaned, Inline: false})
		}
	}

	if opts.amount != "" {
		decimals := int64(opts.tokenDecimals)
		if decimals <= 0 {
			decimals = 18
		}
		symbol := opts.tokenSymbol
		if symbol == "" {
			symbol = getNativeTokenSymbol(opts.chainID)
		}
		if amountStr, err := parseFixedPointString(opts.amount, decimals, -1); err == nil {
			value := fmt.Sprintf("%s %s", amountStr, symbol)
			if opts.showUSD && opts.amountUSD != "" && opts.amountUSD != "0" {
				if amountUsdStr, errUsd := parseFixedPointString(opts.amountUSD, 18, 2); errUsd == nil && amountUsdStr != "0" && amountUsdStr != "0.00" {
					value = fmt.Sprintf("%s %s ($%s USD)", amountStr, symbol, amountUsdStr)
				}
			}
			fields = append(fields, &discordgo.MessageEmbedField{Name: "Amount", Value: value, Inline: true})
		}
	}

	if opts.networkName != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Network", Value: opts.networkName, Inline: true})
	}

	if opts.beneficiary != "" {
		beneficiary, beneficiaryURL := formatAddressLink(opts.beneficiary, opts.chainID)
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Beneficiary", Value: fmt.Sprintf("[%s](%s)", beneficiary, beneficiaryURL), Inline: true})
	}

	if opts.txHash != "" {
		explorerURL := getExplorerURL(opts.chainID, fmt.Sprintf("tx/%s", opts.txHash))
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Transaction", Value: fmt.Sprintf("[Explorer](%s)", explorerURL), Inline: true})
	}

	projectLink := getProjectLink(opts.projectVersion, opts.projectChain, opts.projectID, opts.handle)

	embed := &discordgo.MessageEmbed{
		Title:     fmt.Sprintf("Payment to %s", m.Name),
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: getURLFromURI(m.LogoURI)},
		URL:       projectLink,
		Color:     getProjectColor(opts.projectVersion, opts.projectChain, opts.projectID),
		Fields:    fields,
	}

	if noteImage != "" {
		embed.Image = &discordgo.MessageEmbedImage{URL: noteImage}
	}

	return embed
}

func formatV3Project(event Project, m Metadata) *discordgo.MessageEmbed {
	fields := make([]*discordgo.MessageEmbedField, 0, 5)

	if event.Creator != "" {
		creator, creatorURL := formatAddressLink(event.Creator, 1)

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Creator",
			Value:  fmt.Sprintf("[%s](%s)", creator, creatorURL),
			Inline: true,
		})
	}

	if event.Creator != event.Owner && event.Owner != "" {
		owner, ownerURL := formatAddressLink(event.Owner, 1)

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Owner",
			Value:  fmt.Sprintf("[%s](%s)", owner, ownerURL),
			Inline: true,
		})
	}

	if len(event.InitEvents) > 0 && event.InitEvents[0].TxHash != "" {
		explorerURL := getExplorerURL(1, fmt.Sprintf("tx/%s", event.InitEvents[0].TxHash))

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Transaction",
			Value:  fmt.Sprintf("[Explorer](%s)", explorerURL),
			Inline: true,
		})
	}

	projectLink := getProjectLink(event.Pv, 1, event.ProjectID, event.Handle)
	title := fmt.Sprintf("New project: %s", m.Name)

	// Put tagline directly as description (no label)
	var description string
	if m.ProjectTagline != "" {
		description = m.ProjectTagline
	}

	return &discordgo.MessageEmbed{
		Title:       title,
		Description: description,
		Thumbnail:   &discordgo.MessageEmbedThumbnail{URL: getURLFromURI(m.LogoURI)},
		URL:         projectLink,
		Color:       getProjectColor(event.Pv, 1, event.ProjectID),
		Fields:      fields,
	}
}

func formatBendyProjectGroup(projectGroup []BendyProject, m Metadata) *discordgo.MessageEmbed {
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
		chainName := getChainName(project.ChainID)
		networks = append(networks, chainName)

		// Add project links for each network
		versionStr := strconv.Itoa(project.Version)
		link := getProjectLink(versionStr, project.ChainID, project.ProjectID, project.Handle)
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
		creator, creatorURL := formatAddressLink(firstProject.Creator, firstProject.ChainID)

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Creator",
			Value:  fmt.Sprintf("[%s](%s)", creator, creatorURL),
			Inline: true,
		})
	}

	if firstProject.Creator != firstProject.Owner && firstProject.Owner != "" {
		owner, ownerURL := formatAddressLink(firstProject.Owner, firstProject.ChainID)

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Owner",
			Value:  fmt.Sprintf("[%s](%s)", owner, ownerURL),
			Inline: true,
		})
	}

	if len(firstProject.ProjectCreateEvents.Items) > 0 {
		tx := firstProject.ProjectCreateEvents.Items[0].TxHash
		if tx != "" {
			fields = append(fields, &discordgo.MessageEmbedField{
				Name:   "Transaction",
				Value:  fmt.Sprintf("[Explorer](%s)", getExplorerURL(firstProject.ChainID, fmt.Sprintf("tx/%s", tx))),
				Inline: true,
			})
		}
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
	versionStr := strconv.Itoa(firstProject.Version)
	mainProjectLink := getProjectLink(versionStr, firstProject.ChainID, firstProject.ProjectID, firstProject.Handle)

	return &discordgo.MessageEmbed{
		Title:       title,
		Description: description,
		Thumbnail:   &discordgo.MessageEmbedThumbnail{URL: getURLFromURI(m.LogoURI)},
		URL:         mainProjectLink,
		Color:       getProjectColor(versionStr, firstProject.ChainID, firstProject.ProjectID),
		Fields:      fields,
	}
}

func getProjectLink(version string, chainID int, projectID int, handle string) string {
	// v1 projects use handle
	if version == "1" {
		return fmt.Sprintf("https://juicebox.money/p/%s", handle)
	}

	// v2 and v3 projects use v2/p/ format
	if version == "2" || version == "3" {
		return fmt.Sprintf("https://juicebox.money/v2/p/%d", projectID)
	}

	// v4+ projects share the v{version}/{network}:{projectId} format
	if numericVersion, err := strconv.Atoi(version); err == nil && numericVersion >= 4 {
		return fmt.Sprintf("https://juicebox.money/v%d/%s:%d", numericVersion, getChainShortName(chainID), projectID)
	}

	// Fallback
	log.Printf("Unknown version: %s for project %d\n", version, projectID)
	return fmt.Sprintf("https://juicebox.money/p/%d", projectID)
}

type ChainInfo struct {
	Name         string
	ShortName    string
	ExplorerURL  string
	NativeSymbol string
}

var chains = map[int]ChainInfo{
	1:     {"Ethereum", "eth", "https://etherscan.io", "ETH"},
	10:    {"Optimism", "op", "https://optimistic.etherscan.io", "ETH"},
	8453:  {"Base", "base", "https://basescan.org", "ETH"},
	42161: {"Arbitrum", "arb", "https://arbiscan.io", "ETH"},
}

var addressLabels = map[string]string{
	"0x755ff2f75a0a586ecfa2b9a3c959cb662458a105": "Juicebox Deployer",
}

func getChainName(chainID int) string {
	if chain, ok := chains[chainID]; ok {
		return chain.Name
	}
	return fmt.Sprintf("Chain %d", chainID)
}

func getChainShortName(chainID int) string {
	if chain, ok := chains[chainID]; ok {
		return chain.ShortName
	}
	return fmt.Sprintf("chain%d", chainID)
}

func getNativeTokenSymbol(chainID int) string {
	if chain, ok := chains[chainID]; ok && chain.NativeSymbol != "" {
		return chain.NativeSymbol
	}
	return "ETH"
}

func getExplorerURL(chainID int, path string) string {
	explorer := "https://etherscan.io" // fallback
	if chain, ok := chains[chainID]; ok {
		explorer = chain.ExplorerURL
	}
	return fmt.Sprintf("%s/%s", explorer, path)
}

// Format address as a link with ENS resolution
func formatAddressLink(address string, chainID int) (string, string) {
	// Get display name with combined ENS/truncation logic
	var displayName string
	lower := strings.ToLower(address)
	if label, ok := addressLabels[lower]; ok {
		return label, getExplorerURL(chainID, fmt.Sprintf("address/%s", address))
	}

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

	return displayName, getExplorerURL(chainID, fmt.Sprintf("address/%s", address))
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
func removeIPFSURLs(text string) string {
	result := text
	for _, pattern := range ipfsPatterns {
		result = regexp.MustCompile(pattern).ReplaceAllString(result, "")
	}
	return strings.TrimSpace(regexp.MustCompile(`\s+`).ReplaceAllString(result, " "))
}

// Generate deterministic color based on project identity
func getProjectColor(version string, chainID int, projectID int) int {
	// Create a hash input from version, chainID, and projectID
	input := fmt.Sprintf("%s:%d:%d", version, chainID, projectID)
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

func getMetadataForURI(uri string) (*Metadata, error) {
	metadataURL := getURLFromURI(uri)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
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

func getURLFromURI(uri string) string {
	// Check if the URI is already a URL.
	if len(uri) >= 4 && uri[0:4] == "http" {
		oldEndpoint := "https://jbx.mypinata.cloud/ipfs/"
		if len(uri) >= len(oldEndpoint) && uri[0:len(oldEndpoint)] == oldEndpoint {
			uri = IPFS_ENDPOINT + uri[len(oldEndpoint)-1:]
		}

		return uri
	}

	// Check if the URI starts with ipfs://
	prefix := "ipfs://"
	n := len(prefix)
	if len(uri) >= n && uri[0:n] == prefix {
		uri = uri[n:]
	}

	return IPFS_ENDPOINT + uri
}

func memoizedMetadata(m *MetadataCache, projectID string, metadataURI string, handle string, pv string) Metadata {
	m.Lock()
	cacheValue := m.Map[projectID]

	// If the cache value exists but the IPFS URI has changed, invalidate the currently cached value.
	if cacheValue != nil {
		select {
		case <-cacheValue.ready:
			if cacheValue.MetadataIPFSURI != metadataURI {
				cacheValue = nil
			}
		default:
		}
	}

	// If there is no valid cache value, create a new one.
	if cacheValue == nil {
		// Create placeholder metadata (and new ready chan).
		cacheValue = createPlaceholderCacheValue(projectID, handle, pv)
		m.Map[projectID] = cacheValue
		m.Unlock()

		if metadataURI != "" {
			// If there is a metadata URI, get the metadata from IPFS.
			newMetadata, err := getMetadataForURI(metadataURI)
			if err != nil {
				log.Printf("Failed to fetch metadata for project %s (using placeholder): %v\n", projectID, err)
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

func createPlaceholderCacheValue(projectID string, handle string, pv string) *MetadataCacheValue {
	metadata := &Metadata{Name: fmt.Sprintf("Project %s", projectID)}
	switch pv {
	case "5", "4":
		metadata.Name = fmt.Sprintf("Project %s (v%s)", projectID, pv)
	case "3", "2":
		metadata.InfoURI = fmt.Sprintf("https://juicebox.money/v2/p/%s", projectID)
	case "1":
		metadata.Name += " (v1)"
		metadata.InfoURI = fmt.Sprintf("https://juicebox.money/p/%s", handle)
	}

	return &MetadataCacheValue{
		MetadataIPFSURI: "",
		Metadata:        *metadata,
		ready:           make(chan struct{}),
	}
}
