package main

import "sync"

type GraphQLRequest struct {
	Query string `json:"query"`
}

// V3 PayEvent (from subgraph)
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

type V3PayEventsResponse struct {
	Data struct {
		PayEvents []PayEvent `json:"payEvents"`
	} `json:"data"`
}

// V4 PayEvent (from bendystraw)
type PayEventV4 struct {
	ChainId     int    `json:"chainId"`
	ProjectId   int    `json:"projectId"`
	Amount      string `json:"amount"`
	AmountUsd   string `json:"amountUsd"`
	Timestamp   int    `json:"timestamp"`
	Beneficiary string `json:"beneficiary"`
	TxHash      string `json:"txHash"`
	Memo        string `json:"memo"`
	Project     *struct {
		Handle      string `json:"handle"`
		MetadataUri string `json:"metadataUri"`
		Creator     string `json:"creator"`
		Owner       string `json:"owner"`
	} `json:"project"`
}

type V4PayEventsResponse struct {
	Data struct {
		PayEvents struct {
			Items []PayEventV4 `json:"items"`
		} `json:"payEvents"`
	} `json:"data"`
}

// V3 Project (from subgraph)
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

type V3ProjectsResponse struct {
	Data struct {
		Projects []Project `json:"projects"`
	}
}

// V4 Project (from bendystraw)
type ProjectV4 struct {
	ChainId     int    `json:"chainId"`
	ProjectId   int    `json:"projectId"`
	Handle      string `json:"handle"`
	MetadataUri string `json:"metadataUri"`
	Creator     string `json:"creator"`
	Owner       string `json:"owner"`
}

type V4ProjectsResponse struct {
	Data struct {
		Projects struct {
			Items []ProjectV4 `json:"items"`
		} `json:"projects"`
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
	ready           chan struct{} // Closed when metadata is ready.
}

type MetadataCache struct {
	sync.Mutex // Protects the map.
	Map        map[string]*MetadataCacheValue
}

