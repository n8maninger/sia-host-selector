package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/rodaine/table"
	"github.com/shopspring/decimal"
	"github.com/siacentral/apisdkgo"
	"github.com/siacentral/apisdkgo/sia"
	"go.sia.tech/siad/build"
	"go.sia.tech/siad/modules"
	siaapi "go.sia.tech/siad/node/api/client"
	"go.sia.tech/siad/types"
)

var (
	siaCentralClient = apisdkgo.NewSiaClient()
)

var (
	// minimum of 50 hosts + a few extra for churn, will throw an error if not
	// enough hosts are available
	minHosts = 100
	// $10 USD/TB
	maxDownloadPrice = decimal.NewFromFloat(10)
	// $1.00 USD/TB
	maxUploadPrice = decimal.NewFromFloat(1)
	// $2.00 USD/TB/mo
	maxStorePrice = decimal.NewFromFloat(2)
	// at least a month old (30 days * 144 blocks)
	minAge uint64 = 30 * 144
	// 85% as measured by Sia Central
	minUptime float64 = 80
	// 5Mbps as measured by Sia Central
	//
	// note: I leave this relatively low since not every host has good peering
	// to the central benchmark server
	minDownloadSpeed uint64 = 5e6
	// 1Mbps as measured by Sia Central
	//
	// note: I leave this relatively low since not every host has good peering
	// to the central benchmark server
	minUploadSpeed uint64 = 1e6
)

func formatBpsString(speed decimal.Decimal) string {
	const units = "KMGTPE"
	var factor = decimal.New(1000, 0)

	// short-circuit for < 1000 bits/s
	if speed.Cmp(factor) < 0 {
		return fmt.Sprintf("%v bps", speed.StringFixed(2))
	}

	var i = -1
	for ; speed.Cmp(factor) > 0; i++ {
		speed = speed.Div(factor)
	}
	return fmt.Sprintf("%v %cbps", speed.StringFixed(2), units[i])
}

func formatAge(d time.Duration) string {
	return fmt.Sprintf("%0.2f w", d.Hours()/24/7)
}

func updateHostWhitelist() error {
	sc, _, err := siaCentralClient.GetExchangeRate()
	if err != nil {
		return fmt.Errorf("unable to get exchange rate")
	}

	rate, ok := sc["usd"]
	if !ok || rate <= 0 {
		return fmt.Errorf("usd rate not found or 0")
	}

	rstore, _ := maxStorePrice.Div(decimal.NewFromFloat(rate)).Float64()
	rdown, _ := maxDownloadPrice.Div(decimal.NewFromFloat(rate)).Float64()
	rup, _ := maxUploadPrice.Div(decimal.NewFromFloat(rate)).Float64()
	maxUpPriceSC := types.SiacoinPrecision.MulFloat(rup).Div64(1e12)
	maxDownPriceSC := types.SiacoinPrecision.MulFloat(rdown).Div64(1e12)
	maxStorePriceSC := types.SiacoinPrecision.MulFloat(rstore).Div64(1e12).Div64(4320)

	filter := make(sia.HostFilter)
	filter.WithAcceptingContracts(true)
	filter.WithBenchmarked(true)
	filter.WithMinAge(minAge)
	filter.WithMaxContractPrice(types.SiacoinPrecision.Div64(2))
	filter.WithMaxUploadPrice(maxUpPriceSC)
	filter.WithMaxDownloadPrice(maxDownPriceSC)
	filter.WithMaxStoragePrice(maxStorePriceSC)
	filter.WithMinUptime(minUptime)
	filter.WithMinDownloadSpeed(minDownloadSpeed)
	filter.WithMinUploadSpeed(minUploadSpeed)
	filter.WithSort(sia.HostSortDownloadSpeed, true)

	hosts, err := siaCentralClient.GetActiveHosts(filter, 0, 500)
	if err != nil {
		return fmt.Errorf("unable to get filtered hosts: %w", err)
	}

	var contractPrice, storagePrice, downloadPrice, uploadPrice struct{ min, max, avg types.Currency }
	var uptime, downloadSpeed, uploadSpeed struct{ min, max, avg decimal.Decimal }
	var ages struct{ min, max, avg time.Duration }
	keys := []types.SiaPublicKey{}

	if minHosts > len(hosts) {
		return fmt.Errorf("not enough hosts need %d got %d", minHosts, len(hosts))
	}

	for i, host := range hosts {
		contractPrice.avg = contractPrice.avg.Add(host.Settings.ContractPrice)
		storagePrice.avg = storagePrice.avg.Add(host.Settings.StoragePrice)
		downloadPrice.avg = downloadPrice.avg.Add(host.Settings.DownloadBandwidthPrice)
		uploadPrice.avg = uploadPrice.avg.Add(host.Settings.UploadBandwidthPrice)
		uptime.avg = uptime.avg.Add(decimal.NewFromFloat32(host.EstimatedUptime))

		upSeconds := decimal.New(int64(host.Benchmark.UploadTime), 0).Div(decimal.New(1000, 0))
		downSeconds := decimal.New(int64(host.Benchmark.DownloadTime), 0).Div(decimal.New(1000, 0))
		upBps := decimal.New(int64(host.Benchmark.DataSize)*8, 0).Div(upSeconds)
		downBps := decimal.New(int64(host.Benchmark.DataSize)*8, 0).Div(downSeconds)
		downloadSpeed.avg = downloadSpeed.avg.Add(downBps)
		uploadSpeed.avg = uploadSpeed.avg.Add(upBps)

		age := time.Since(host.FirstSeenTimestamp)
		ages.avg += age

		if i == 0 {
			contractPrice.min = host.Settings.ContractPrice
			storagePrice.min = host.Settings.StoragePrice
			downloadPrice.min = host.Settings.DownloadBandwidthPrice
			uploadPrice.min = host.Settings.UploadBandwidthPrice
			uptime.min = decimal.NewFromFloat32(host.EstimatedUptime)
			downloadSpeed.min = downBps
			uploadSpeed.min = upBps
			ages.min = age
		}

		if host.Settings.ContractPrice.Cmp(contractPrice.min) < 0 {
			contractPrice.min = host.Settings.ContractPrice
		}
		if host.Settings.ContractPrice.Cmp(contractPrice.max) > 0 {
			contractPrice.max = host.Settings.ContractPrice
		}

		if host.Settings.StoragePrice.Cmp(storagePrice.min) < 0 {
			storagePrice.min = host.Settings.StoragePrice
		}
		if host.Settings.StoragePrice.Cmp(storagePrice.max) > 0 {
			storagePrice.max = host.Settings.StoragePrice
		}

		if host.Settings.DownloadBandwidthPrice.Cmp(downloadPrice.min) < 0 {
			downloadPrice.min = host.Settings.DownloadBandwidthPrice
		}
		if host.Settings.DownloadBandwidthPrice.Cmp(downloadPrice.max) > 0 {
			downloadPrice.max = host.Settings.DownloadBandwidthPrice
		}

		if host.Settings.UploadBandwidthPrice.Cmp(uploadPrice.min) < 0 {
			uploadPrice.min = host.Settings.UploadBandwidthPrice
		}
		if host.Settings.UploadBandwidthPrice.Cmp(uploadPrice.max) > 0 {
			uploadPrice.max = host.Settings.UploadBandwidthPrice
		}

		if n, _ := uptime.min.Float64(); n > float64(host.EstimatedUptime) {
			uptime.min = decimal.NewFromFloat32(host.EstimatedUptime)
		}
		if n, _ := uptime.max.Float64(); n < float64(host.EstimatedUptime) {
			uptime.max = decimal.NewFromFloat32(host.EstimatedUptime)
		}

		if downloadSpeed.min.Cmp(downBps) > 0 {
			downloadSpeed.min = downBps
		}
		if downloadSpeed.max.Cmp(downBps) < 0 {
			downloadSpeed.max = downBps
		}

		if uploadSpeed.min.Cmp(upBps) > 0 {
			uploadSpeed.min = upBps
		}
		if uploadSpeed.max.Cmp(upBps) < 0 {
			uploadSpeed.max = upBps
		}

		if ages.min > age {
			ages.min = age
		}
		if ages.max < age {
			ages.max = age
		}

		var spk types.SiaPublicKey
		if err := spk.LoadString(host.PublicKey); err != nil {
			return fmt.Errorf("unable to load public key: %w", err)
		}
		keys = append(keys, spk)
	}

	contractPrice.avg = contractPrice.avg.Div64(uint64(len(hosts)))
	storagePrice.avg = storagePrice.avg.Div64(uint64(len(hosts)))
	downloadPrice.avg = downloadPrice.avg.Div64(uint64(len(hosts)))
	uploadPrice.avg = uploadPrice.avg.Div64(uint64(len(hosts)))
	uptime.avg = uptime.avg.Div(decimal.New(int64(len(hosts)), 0))
	downloadSpeed.avg = downloadSpeed.avg.Div(decimal.New(int64(len(hosts)), 0))
	uploadSpeed.avg = uploadSpeed.avg.Div(decimal.New(int64(len(hosts)), 0))
	ages.avg = ages.avg / time.Duration(len(hosts))

	log.Printf("Matching %d hosts", len(hosts))
	tbl := table.New("", "Min", "Avg", "Max")
	tbl.AddRow("Contract Price", contractPrice.min.HumanString(), contractPrice.avg.HumanString(), contractPrice.max.HumanString())
	tbl.AddRow("Storage", storagePrice.min.Mul64(1e12).Mul64(4320).HumanString(), storagePrice.avg.Mul64(1e12).Mul64(4320).HumanString(), storagePrice.max.Mul64(1e12).Mul64(4320).HumanString())
	tbl.AddRow("Download", downloadPrice.min.Mul64(1e12).HumanString(), downloadPrice.avg.Mul64(1e12).HumanString(), downloadPrice.max.Mul64(1e12).HumanString())
	tbl.AddRow("Upload", uploadPrice.min.Mul64(1e12).HumanString(), uploadPrice.avg.Mul64(1e12).HumanString(), uploadPrice.max.Mul64(1e12).HumanString())
	tbl.AddRow("Uptime", uptime.min.StringFixed(2)+"%", uptime.avg.StringFixed(2)+"%", uptime.max.StringFixed(2)+"%")
	tbl.AddRow("Age", formatAge(ages.min), formatAge(ages.avg), formatAge(ages.max))
	tbl.AddRow("Download Speed", formatBpsString(downloadSpeed.min), formatBpsString(downloadSpeed.avg), formatBpsString(downloadSpeed.max))
	tbl.AddRow("Upload Speed", formatBpsString(uploadSpeed.min), formatBpsString(uploadSpeed.avg), formatBpsString(uploadSpeed.max))
	tbl.Print()

	siaPass, err := build.APIPassword()
	if err != nil {
		return fmt.Errorf("unable to get api password: %w", err)
	}

	siaAddr := os.Getenv("SIA_API_ADDRESS")
	if len(siaAddr) == 0 {
		siaAddr = "localhost:9980"
	}

	siaClient := siaapi.New(siaapi.Options{
		Address:  siaAddr,
		Password: siaPass,
	})

	err = siaClient.HostDbFilterModePost(modules.HostDBActiveWhitelist, keys, nil)
	if err != nil {
		return fmt.Errorf("unable to update hostdb filter: %w", err)
	}

	return nil
}

func main() {

	for {
		log.Println("Updating Whitelist")
		if err := updateHostWhitelist(); err != nil {
			log.Println("[WARN]", err)
		}
		time.Sleep(time.Hour * 8)
	}
}
