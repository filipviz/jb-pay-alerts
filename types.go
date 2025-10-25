package main

import "sync"

type GraphQLRequest struct {
	Query string `json:"query"`
}

// V3 PayEvent (from subgraph)
type PayEvent struct {
	Pv          string `json:"pv"`
	ProjectID   int    `json:"projectId"`
	Amount      string `json:"amount"`
	AmountUSD   string `json:"amountUSD"`
	Timestamp   int    `json:"timestamp"`
	Beneficiary string `json:"beneficiary"`
	Note        string `json:"note"`
	TxHash      string `json:"txHash"`
	Project     struct {
		MetadataURI string `json:"metadataUri"`
		Handle      string `json:"handle"`
	} `json:"project"`
}

type V3PayEventsResponse struct {
	Data struct {
		PayEvents []PayEvent `json:"payEvents"`
	} `json:"data"`
}

// Bendystraw PayEvent (supports Juicebox v4 and v5 projects)
type BendyPayEvent struct {
	ChainID     int    `json:"chainId"`
	ProjectID   int    `json:"projectId"`
	Version     int    `json:"version"`
	Amount      string `json:"amount"`
	AmountUSD   string `json:"amountUsd"`
	Timestamp   int    `json:"timestamp"`
	Beneficiary string `json:"beneficiary"`
	TxHash      string `json:"txHash"`
	Memo        string `json:"memo"`
	Caller      string `json:"caller"`
	From        string `json:"from"`
	Project     *struct {
		Handle        string  `json:"handle"`
		MetadataURI   string  `json:"metadataUri"`
		Creator       string  `json:"creator"`
		Owner         string  `json:"owner"`
		IsRevnet      bool    `json:"isRevnet"`
		Version       int     `json:"version"`
		SuckerGroupID string  `json:"suckerGroupId"`
		Token         string  `json:"token"`
		TokenSymbol   *string `json:"tokenSymbol"`
		Decimals      *int    `json:"decimals"`
	} `json:"project"`
}

type BendyPayEventsResponse struct {
	Data struct {
		PayEvents struct {
			Items []BendyPayEvent `json:"items"`
		} `json:"payEvents"`
	} `json:"data"`
}

// V3 Project (from subgraph)
type Project struct {
	Pv          string `json:"pv"`
	Handle      string `json:"handle"`
	ProjectID   int    `json:"projectId"`
	MetadataURI string `json:"metadataUri"`
	Creator     string `json:"creator"`
	Owner       string `json:"owner"`
	InitEvents  []struct {
		TxHash string `json:"txHash"`
	} `json:"initEvents"`
}

type V3ProjectsResponse struct {
	Data struct {
		Projects []Project `json:"projects"`
	}
}

// Bendystraw Project (supports Juicebox v4 and v5 projects)
type BendyProject struct {
	ChainID             int    `json:"chainId"`
	ProjectID           int    `json:"projectId"`
	Version             int    `json:"version"`
	Handle              string `json:"handle"`
	MetadataURI         string `json:"metadataUri"`
	Creator             string `json:"creator"`
	Owner               string `json:"owner"`
	IsRevnet            bool   `json:"isRevnet"`
	SuckerGroupID       string `json:"suckerGroupId"`
	ProjectCreateEvents struct {
		Items []struct {
			TxHash string `json:"txHash"`
		} `json:"items"`
	} `json:"projectCreateEvents"`
}

type BendyProjectsResponse struct {
	Data struct {
		Projects struct {
			Items []BendyProject `json:"items"`
		} `json:"projects"`
	} `json:"data"`
}

type Metadata struct {
	Name           string `json:"name"`
	InfoURI        string `json:"infoUri"`
	LogoURI        string `json:"logoUri"`
	Description    string `json:"description"`
	ProjectTagline string `json:"projectTagline"`
}

type MetadataCacheValue struct {
	MetadataIPFSURI string
	Metadata        Metadata
	ready           chan struct{} // Closed when metadata is ready.
}

type MetadataCache struct {
	sync.Mutex // Protects the map.
	Map        map[string]*MetadataCacheValue
}
