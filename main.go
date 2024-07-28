package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/sagernet/sing-box/common/geosite"
	"github.com/sagernet/sing-box/common/srs"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"

	"github.com/google/go-github/v45/github"
	"github.com/v2fly/v2ray-core/v5/app/router/routercommon"
	"google.golang.org/protobuf/proto"
)

var githubClient *github.Client

func init() {
	accessToken, loaded := os.LookupEnv("ACCESS_TOKEN")
	if !loaded {
		githubClient = github.NewClient(nil)
		return
	}
	transport := &github.BasicAuthTransport{
		Username: accessToken,
	}
	githubClient = github.NewClient(transport.Client())
}

func fetch(from string) (*github.RepositoryRelease, error) {
	names := strings.SplitN(from, "/", 2)
	latestRelease, _, err := githubClient.Repositories.GetLatestRelease(context.Background(), names[0], names[1])
	if err != nil {
		return nil, err
	}
	return latestRelease, err
}

func get(downloadURL *string) ([]byte, error) {
	log.Info("download ", *downloadURL)
	response, err := http.Get(*downloadURL)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	return io.ReadAll(response.Body)
}

func download(release *github.RepositoryRelease) ([]byte, error) {
	geositeAsset := common.Find(release.Assets, func(it *github.ReleaseAsset) bool {
		return *it.Name == "geosite.dat"
	})
	geositeChecksumAsset := common.Find(release.Assets, func(it *github.ReleaseAsset) bool {
		return *it.Name == "geosite.dat.sha256sum"
	})
	if geositeAsset == nil {
		return nil, E.New("geosite asset not found in upstream release ", release.Name)
	}
	if geositeChecksumAsset == nil {
		return nil, E.New("geosite asset not found in upstream release ", release.Name)
	}
	data, err := get(geositeAsset.BrowserDownloadURL)
	if err != nil {
		return nil, err
	}
	remoteChecksum, err := get(geositeChecksumAsset.BrowserDownloadURL)
	if err != nil {
		return nil, err
	}
	checksum := sha256.Sum256(data)
	if hex.EncodeToString(checksum[:]) != string(remoteChecksum[:64]) {
		return nil, E.New("checksum mismatch")
	}
	return data, nil
}

func parse(vGeositeData []byte) (map[string][]geosite.Item, error) {
	vGeositeList := routercommon.GeoSiteList{}
	err := proto.Unmarshal(vGeositeData, &vGeositeList)
	if err != nil {
		return nil, err
	}
	domainMap := make(map[string][]geosite.Item)
	for _, vGeositeEntry := range vGeositeList.Entry {
		code := strings.ToLower(vGeositeEntry.CountryCode)
		domains := make([]geosite.Item, 0, len(vGeositeEntry.Domain)*2)
		attributes := make(map[string][]*routercommon.Domain)
		for _, domain := range vGeositeEntry.Domain {
			if len(domain.Attribute) > 0 {
				for _, attribute := range domain.Attribute {
					attributes[attribute.Key] = append(attributes[attribute.Key], domain)
				}
			}
			switch domain.Type {
			case routercommon.Domain_Plain:
				domains = append(domains, geosite.Item{
					Type:  geosite.RuleTypeDomainKeyword,
					Value: domain.Value,
				})
			case routercommon.Domain_Regex:
				domains = append(domains, geosite.Item{
					Type:  geosite.RuleTypeDomainRegex,
					Value: domain.Value,
				})
			case routercommon.Domain_RootDomain:
				if strings.Contains(domain.Value, ".") {
					domains = append(domains, geosite.Item{
						Type:  geosite.RuleTypeDomain,
						Value: domain.Value,
					})
				}
				domains = append(domains, geosite.Item{
					Type:  geosite.RuleTypeDomainSuffix,
					Value: "." + domain.Value,
				})
			case routercommon.Domain_Full:
				domains = append(domains, geosite.Item{
					Type:  geosite.RuleTypeDomain,
					Value: domain.Value,
				})
			}
		}
		domainMap[code] = common.Uniq(domains)
		for attribute, attributeEntries := range attributes {
			attributeDomains := make([]geosite.Item, 0, len(attributeEntries)*2)
			for _, domain := range attributeEntries {
				switch domain.Type {
				case routercommon.Domain_Plain:
					attributeDomains = append(attributeDomains, geosite.Item{
						Type:  geosite.RuleTypeDomainKeyword,
						Value: domain.Value,
					})
				case routercommon.Domain_Regex:
					attributeDomains = append(attributeDomains, geosite.Item{
						Type:  geosite.RuleTypeDomainRegex,
						Value: domain.Value,
					})
				case routercommon.Domain_RootDomain:
					if strings.Contains(domain.Value, ".") {
						attributeDomains = append(attributeDomains, geosite.Item{
							Type:  geosite.RuleTypeDomain,
							Value: domain.Value,
						})
					}
					attributeDomains = append(attributeDomains, geosite.Item{
						Type:  geosite.RuleTypeDomainSuffix,
						Value: "." + domain.Value,
					})
				case routercommon.Domain_Full:
					attributeDomains = append(attributeDomains, geosite.Item{
						Type:  geosite.RuleTypeDomain,
						Value: domain.Value,
					})
				}
			}
			domainMap[code+"@"+attribute] = common.Uniq(attributeDomains)
		}
	}
	return domainMap, nil
}

func buildItemList(t geosite.ItemType, strings []string) []geosite.Item {
	var items []geosite.Item
	for _, str := range strings {
		items = append(items, geosite.Item{
			Type:  t,
			Value: str,
		})
	}
	return items
}

func merge(items1 []geosite.Item, items2 []geosite.Item) []geosite.Item {
	itemMap := make(map[geosite.Item]struct{})
	for _, item := range items1 {
		itemMap[item] = struct{}{}
	}
	for _, item := range items2 {
		itemMap[item] = struct{}{}
	}
	var mergedItems []geosite.Item
	for item := range itemMap {
		mergedItems = append(mergedItems, item)
	}
	return mergedItems
}

func remove(items1 []geosite.Item, items2 []geosite.Item) []geosite.Item {
	itemMap := make(map[geosite.Item]struct{})
	for _, item := range items2 {
		itemMap[item] = struct{}{}
	}
	var result []geosite.Item
	for _, item := range items1 {
		if _, found := itemMap[item]; !found {
			result = append(result, item)
		}
	}
	return result
}

func handle(data map[string][]geosite.Item) map[string][]geosite.Item {
	gfwKeywords := []string{"1e100", "abema", "appledaily", "avtb", "beetalk", "blogspot", "dropbox", "facebook", "fbcdn", "github", "gmail", "google", "instagram", "porn", "sci-hub", "spotify", "twitter", "whatsapp", "youtube", "uk-live", "1drv", "onedrive", "skydrive", "porn", "ttvnw"}

	gfw := remove(data["gfw"], data["telegram"])
	gfw = merge(gfw, buildItemList(geosite.RuleTypeDomainKeyword, gfwKeywords))

	result := make(map[string][]geosite.Item)
	result["gfw-!tg"] = gfw
	result["github"] = data["github"]
	return result
}

func generate(release *github.RepositoryRelease, output string, ruleSetOutput string) error {
	vData, err := download(release)
	if err != nil {
		return err
	}
	domainMap, err := parse(vData)
	if err != nil {
		return err
	}

	domainMap = handle(domainMap)

	outputPath, _ := filepath.Abs(output)
	os.Stderr.WriteString("write " + outputPath + "\n")
	outputFile, err := os.Create(output)
	if err != nil {
		return err
	}
	defer outputFile.Close()
	writer := bufio.NewWriter(outputFile)
	err = geosite.Write(writer, domainMap)
	if err != nil {
		return err
	}
	err = writer.Flush()
	if err != nil {
		return err
	}
	os.RemoveAll(ruleSetOutput)
	err = os.MkdirAll(ruleSetOutput, 0o755)
	if err != nil {
		return err
	}
	for code, domains := range domainMap {
		var headlessRule option.DefaultHeadlessRule
		defaultRule := geosite.Compile(domains)
		headlessRule.Domain = defaultRule.Domain
		headlessRule.DomainSuffix = defaultRule.DomainSuffix
		headlessRule.DomainKeyword = defaultRule.DomainKeyword
		headlessRule.DomainRegex = defaultRule.DomainRegex
		var plainRuleSet option.PlainRuleSet
		plainRuleSet.Rules = []option.HeadlessRule{
			{
				Type:           C.RuleTypeDefault,
				DefaultOptions: headlessRule,
			},
		}
		srsPath, _ := filepath.Abs(filepath.Join(ruleSetOutput, "geosite-"+code+".srs"))
		// os.Stderr.WriteString("write " + srsPath + "\n")
		var (
			outputRuleSet *os.File
		)
		outputRuleSet, err = os.Create(srsPath)
		if err != nil {
			return err
		}
		err = srs.Write(outputRuleSet, plainRuleSet, false)
		outputRuleSet.Close()
		if err != nil {
			return err
		}

		bs, err := json.MarshalIndent(option.PlainRuleSetCompat{
			Version: 1,
			Options: plainRuleSet,
		}, "", "  ")
		if err != nil {
			return err
		}
		os.WriteFile(filepath.Join(ruleSetOutput, "geosite-"+code+".json"), bs, 0o644)
	}
	return nil
}

func setActionOutput(name string, content string) {
	os.Stdout.WriteString("::set-output name=" + name + "::" + content + "\n")
}

func release(source string, destination string, output string, ruleSetOutput string) error {
	sourceRelease, err := fetch(source)
	if err != nil {
		return err
	}
	sourceReleaseKeyword := sourceRelease.TagName // using tag
	destinationRelease, err := fetch(destination)
	if err != nil {
		log.Warn("missing destination latest release")
	} else {
		if os.Getenv("NO_SKIP") != "true" && strings.Contains(*destinationRelease.Name, *sourceReleaseKeyword) {
			log.Info("already latest")
			setActionOutput("skip", "true")
			return nil
		}
	}
	err = generate(sourceRelease, output, ruleSetOutput)
	if err != nil {
		return err
	}
	setActionOutput("tag", *sourceReleaseKeyword)
	return nil
}

func main() {
	err := release(
		"Loyalsoldier/v2ray-rules-dat",
		"dongxinb/geosite",
		"geosite.db",
		"rule-set",
	)
	if err != nil {
		log.Fatal(err)
	}
}
