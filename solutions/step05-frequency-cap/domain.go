// domain.go — Creative / SlotID / LineItem / Inventory
//
// Step 03 の LineItem 階層に、Step 05 で FreqCapPerDay を追加。
package main

type Creative struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	ImageURL string `json:"image_url"`
	ClickURL string `json:"click_url"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

type SlotID string

const (
	SlotMainRectangle  SlotID = "main-rectangle"
	SlotTopBanner      SlotID = "top-banner"
	SlotSideSkyscraper SlotID = "side-skyscraper"
)

// LineItem: ターゲティング + 入札額 + Creative + フリークエンシーキャップ。
type LineItem struct {
	ID            string    `json:"id"`
	Slot          SlotID    `json:"slot"`
	Targeting     Targeting `json:"targeting"`
	BidCPM        int       `json:"bid_cpm"`
	FreqCapPerDay int       `json:"freq_cap_per_day"` // 0 = 制限なし
	Creative      Creative  `json:"creative"`
}

type Inventory []LineItem

func (inv Inventory) BySlot(slot SlotID) []LineItem {
	out := make([]LineItem, 0, len(inv))
	for _, li := range inv {
		if li.Slot == slot {
			out = append(out, li)
		}
	}
	return out
}

func defaultInventory() Inventory {
	return Inventory{
		{
			ID:   "li-acme-jp-mobile",
			Slot: SlotMainRectangle,
			Targeting: Targeting{
				Countries: Include([]string{"JP"}),
				Devices:   Include([]string{"mobile"}),
			},
			BidCPM:        300,
			FreqCapPerDay: 3,
			Creative: Creative{
				ID: "cr-acme-jp-mobile", Title: "Acme JP モバイル",
				ImageURL: "https://placehold.co/300x250/orange/white?text=Acme+JP+mobile",
				ClickURL: "https://example.com/acme/jp",
				Width:    300, Height: 250,
			},
		},
		{
			ID:   "li-acme-jp-desktop",
			Slot: SlotMainRectangle,
			Targeting: Targeting{
				Countries: Include([]string{"JP"}),
				Devices:   Include([]string{"desktop"}),
			},
			BidCPM:        250,
			FreqCapPerDay: 3,
			Creative: Creative{
				ID: "cr-acme-jp-desktop", Title: "Acme JP デスクトップ",
				ImageURL: "https://placehold.co/300x250/orange/white?text=Acme+JP+desktop",
				ClickURL: "https://example.com/acme/jp",
				Width:    300, Height: 250,
			},
		},
		{
			ID:   "li-globex-jp",
			Slot: SlotMainRectangle,
			Targeting: Targeting{
				Countries: Include([]string{"JP"}),
			},
			BidCPM:        150,
			FreqCapPerDay: 0, // 無制限
			Creative: Creative{
				ID: "cr-globex-jp", Title: "Globex JP",
				ImageURL: "https://placehold.co/300x250/brown/white?text=Globex+JP",
				ClickURL: "https://example.com/globex/jp",
				Width:    300, Height: 250,
			},
		},
		{
			ID:   "li-house-any",
			Slot: SlotMainRectangle,
			// targeting なし → 常に候補
			BidCPM:        10,
			FreqCapPerDay: 0,
			Creative: Creative{
				ID: "cr-house", Title: "House Ad",
				ImageURL: "https://placehold.co/300x250/gray/white?text=House+Ad",
				ClickURL: "https://example.com/house",
				Width:    300, Height: 250,
			},
		},
		{
			ID:   "li-banner-acme-jp",
			Slot: SlotTopBanner,
			Targeting: Targeting{
				Countries: Include([]string{"JP"}),
			},
			BidCPM:        180,
			FreqCapPerDay: 5,
			Creative: Creative{
				ID: "cr-banner-acme", Title: "Acme Banner",
				ImageURL: "https://placehold.co/728x90/orange/white?text=Acme+Banner",
				ClickURL: "https://example.com/acme/banner",
				Width:    728, Height: 90,
			},
		},
		{
			ID:   "li-banner-house",
			Slot: SlotTopBanner,
			// targeting なし
			BidCPM:        10,
			FreqCapPerDay: 0,
			Creative: Creative{
				ID: "cr-banner-house", Title: "House Banner",
				ImageURL: "https://placehold.co/728x90/gray/white?text=House+Banner",
				ClickURL: "https://example.com/house/banner",
				Width:    728, Height: 90,
			},
		},
		// side-skyscraper は意図的に在庫を持たせない → no-fill (204) の確認用。
	}
}
