package backtest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHistoricalDataDownloadAndVerification(t *testing.T) {
	dataDir := filepath.Join("..", "..", "data", "historical")
	_ = os.MkdirAll(dataDir, 0755)

	d := NewDownloader(dataDir)
	err := d.DownloadFullDataset()
	if err != nil {
		t.Fatalf("DownloadFullDataset failed: %v", err)
	}

	// 1. Verify BTC CSV
	btcPath := filepath.Join(dataDir, "btc_daily_full.csv")
	btcBars, err := LoadHistoricalCSV(btcPath)
	if err != nil {
		t.Fatalf("Failed to load BTC CSV: %v", err)
	}
	if len(btcBars) < 5000 {
		t.Fatalf("Expected at least 5000 BTC bars, got %d", len(btcBars))
	}

	t.Logf("Loaded %d BTC bars from %s to %s", len(btcBars), btcBars[0].Date, btcBars[len(btcBars)-1].Date)

	// Check BTC milestones
	var found2011, found2013, found2017, found2018, found2021, found2022, found2024 bool
	for _, b := range btcBars {
		if b.Date == "2011-06-08" { // Peak of 2011 run ~$25-$32
			if b.Close < 10.0 || b.Close > 40.0 {
				t.Errorf("Unexpected 2011 BTC price: %.2f", b.Close)
			}
			found2011 = true
		}
		if b.Date == "2013-11-30" { // Peak of 2013 run ~$1,000
			if b.Close < 800.0 || b.Close > 1300.0 {
				t.Errorf("Unexpected 2013 BTC price: %.2f", b.Close)
			}
			found2013 = true
		}
		if b.Date == "2017-12-17" { // 2017 ATH ~$19k-$20k
			if b.High < 18000.0 {
				t.Errorf("Expected 2017 ATH > 18000, got high %.2f", b.High)
			}
			found2017 = true
		}
		if b.Date == "2018-12-15" { // 2018 bear market bottom ~$3,200
			if b.Low < 2500.0 || b.High > 4000.0 {
				t.Errorf("Unexpected 2018 bottom BTC price: %.2f - %.2f", b.Low, b.High)
			}
			found2018 = true
		}
		if b.Date == "2021-11-10" { // 2021 ATH ~$69k
			if b.High < 65000.0 {
				t.Errorf("Expected 2021 ATH > 65000, got high %.2f", b.High)
			}
			found2021 = true
		}
		if b.Date == "2022-11-21" { // 2022 FTX crash bottom ~$15.5k-$16.5k
			if b.Low < 14000.0 || b.High > 18000.0 {
				t.Errorf("Unexpected 2022 bottom BTC price: %.2f - %.2f", b.Low, b.High)
			}
			found2022 = true
		}
		if b.Date >= "2024-11-01" && b.Close >= 70000.0 {
			found2024 = true
		}
	}

	if !found2011 {
		t.Errorf("Did not verify 2011 milestone")
	}
	if !found2013 {
		t.Errorf("Did not verify 2013 milestone")
	}
	if !found2017 {
		t.Errorf("Did not verify 2017 milestone")
	}
	if !found2018 {
		t.Errorf("Did not verify 2018 milestone")
	}
	if !found2021 {
		t.Errorf("Did not verify 2021 milestone")
	}
	if !found2022 {
		t.Errorf("Did not verify 2022 milestone")
	}
	if !found2024 {
		t.Errorf("Did not verify 2024+ milestone")
	}

	// 2. Verify ETH CSV
	ethPath := filepath.Join(dataDir, "eth_daily_full.csv")
	ethBars, err := LoadHistoricalCSV(ethPath)
	if err != nil {
		t.Fatalf("Failed to load ETH CSV: %v", err)
	}
	if len(ethBars) < 3500 {
		t.Fatalf("Expected at least 3500 ETH bars, got %d", len(ethBars))
	}

	t.Logf("Loaded %d ETH bars from %s to %s", len(ethBars), ethBars[0].Date, ethBars[len(ethBars)-1].Date)

	// Check ETH milestones
	var foundETH2015, foundETH2018, foundETH2021 bool
	for _, b := range ethBars {
		if b.Date == "2015-08-08" {
			if b.Close < 0.5 || b.Close > 3.0 {
				t.Errorf("Unexpected 2015 ETH launch price: %.2f", b.Close)
			}
			foundETH2015 = true
		}
		if b.Date == "2018-01-13" { // 2018 ETH ATH ~$1,300-$1,400
			if b.High < 1200.0 {
				t.Errorf("Expected 2018 ETH ATH > 1200, got high %.2f", b.High)
			}
			foundETH2018 = true
		}
		if b.Date == "2021-11-10" { // 2021 ETH ATH ~$4,700-$4,800
			if b.High < 4500.0 {
				t.Errorf("Expected 2021 ETH ATH > 4500, got high %.2f", b.High)
			}
			foundETH2021 = true
		}
	}

	if !foundETH2015 {
		t.Errorf("Did not verify ETH 2015 launch")
	}
	if !foundETH2018 {
		t.Errorf("Did not verify ETH 2018 ATH")
	}
	if !foundETH2021 {
		t.Errorf("Did not verify ETH 2021 ATH")
	}

	// 3. Verify Volatility and VPIN calculators
	sampleBar := btcBars[len(btcBars)-1]
	pVol := sampleBar.ParkinsonVol()
	gkVol := sampleBar.GarmanKlassVol()
	vpin := sampleBar.VPIN()

	if pVol <= 0 || pVol > 1.0 {
		t.Errorf("Invalid Parkinson volatility: %.4f", pVol)
	}
	if gkVol <= 0 || gkVol > 1.0 {
		t.Errorf("Invalid Garman-Klass volatility: %.4f", gkVol)
	}
	if vpin < 0 || vpin > 1.0 {
		t.Errorf("Invalid VPIN: %.4f", vpin)
	}
	t.Logf("Sample Bar (%s Close=%.2f): Parkinson Vol=%.2f%%, Garman-Klass Vol=%.2f%%, VPIN=%.2f",
		sampleBar.Date, sampleBar.Close, pVol*100, gkVol*100, vpin)
}
