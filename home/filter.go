package home

import (
	"bufio"
	"fmt"
	"hash/crc32"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AdguardTeam/AdGuardHome/dnsfilter"
	"github.com/AdguardTeam/AdGuardHome/util"
	"github.com/AdguardTeam/golibs/log"
)

var (
	nextFilterID      = time.Now().Unix() // semi-stable way to generate an unique ID
	filterTitleRegexp = regexp.MustCompile(`^! Title: +(.*)$`)
	refreshStatus     uint32 // 0:none; 1:in progress
	refreshLock       sync.Mutex
)

func initFiltering() {
	_ = os.MkdirAll(filepath.Join(Context.getDataDir(), filterDir), 0755)
	loadFilters(config.Filters)
	loadFilters(config.WhitelistFilters)
	deduplicateFilters()
	updateUniqueFilterID(config.Filters)
	updateUniqueFilterID(config.WhitelistFilters)
}

func startFiltering() {
	// Here we should start updating filters,
	//  but currently we can't wake up the periodic task to do so.
	// So for now we just start this periodic task from here.
	go periodicallyRefreshFilters()
}

// CloseFiltering - close filters
func CloseFiltering() {
}

func defaultFilters() []filter {
	return []filter{
		{Filter: dnsfilter.Filter{ID: 1}, Enabled: true, URL: "https://adguardteam.github.io/AdGuardSDNSFilter/Filters/filter.txt", Name: "AdGuard Simplified Domain Names filter"},
		{Filter: dnsfilter.Filter{ID: 2}, Enabled: false, URL: "https://adaway.org/hosts.txt", Name: "AdAway"},
		{Filter: dnsfilter.Filter{ID: 3}, Enabled: false, URL: "https://hosts-file.net/ad_servers.txt", Name: "hpHosts - Ad and Tracking servers only"},
		{Filter: dnsfilter.Filter{ID: 4}, Enabled: false, URL: "https://www.malwaredomainlist.com/hostslist/hosts.txt", Name: "MalwareDomainList.com Hosts List"},
	}
}

// field ordering is important -- yaml fields will mirror ordering from here
type filter struct {
	Enabled     bool
	URL         string
	Name        string    `yaml:"name"`
	RulesCount  int       `yaml:"-"`
	LastUpdated time.Time `yaml:"-"`
	checksum    uint32    // checksum of the file data
	white       bool

	dnsfilter.Filter `yaml:",inline"`
}

// Creates a helper object for working with the user rules
func userFilter() filter {
	f := filter{
		// User filter always has constant ID=0
		Enabled: true,
	}
	f.Filter.Data = []byte(strings.Join(config.UserRules, "\n"))
	return f
}

const (
	statusFound          = 1
	statusEnabledChanged = 2
	statusURLChanged     = 4
	statusURLExists      = 8
	statusUpdateRequired = 0x10
)

// Update properties for a filter specified by its URL
// Return status* flags.
func filterSetProperties(url string, newf filter, whitelist bool) int {
	r := 0
	config.Lock()
	defer config.Unlock()

	filters := &config.Filters
	if whitelist {
		filters = &config.WhitelistFilters
	}

	for i := range *filters {
		f := &(*filters)[i]
		if f.URL != url {
			continue
		}

		log.Debug("filter: set properties: %s: {%s %s %v}",
			f.URL, newf.Name, newf.URL, newf.Enabled)
		f.Name = newf.Name

		if f.URL != newf.URL {
			r |= statusURLChanged | statusUpdateRequired
			if filterExistsNoLock(newf.URL) {
				return statusURLExists
			}
			f.URL = newf.URL
			f.unload()
			f.LastUpdated = time.Time{}
			f.checksum = 0
			f.RulesCount = 0
		}

		if f.Enabled != newf.Enabled {
			r |= statusEnabledChanged
			f.Enabled = newf.Enabled
			if f.Enabled {
				if (r & statusURLChanged) == 0 {
					e := f.load()
					if e != nil {
						// This isn't a fatal error,
						//  because it may occur when someone removes the file from disk.
						f.LastUpdated = time.Time{}
						f.checksum = 0
						f.RulesCount = 0
						r |= statusUpdateRequired
					}
				}
			} else {
				f.unload()
			}
		}

		return r | statusFound
	}
	return 0
}

// Return TRUE if a filter with this URL exists
func filterExists(url string) bool {
	config.RLock()
	r := filterExistsNoLock(url)
	config.RUnlock()
	return r
}

func filterExistsNoLock(url string) bool {
	for _, f := range config.Filters {
		if f.URL == url {
			return true
		}
	}
	for _, f := range config.WhitelistFilters {
		if f.URL == url {
			return true
		}
	}
	return false
}

// Add a filter
// Return FALSE if a filter with this URL exists
func filterAdd(f filter) bool {
	config.Lock()
	defer config.Unlock()

	// Check for duplicates
	if filterExistsNoLock(f.URL) {
		return false
	}

	if f.white {
		config.WhitelistFilters = append(config.WhitelistFilters, f)
	} else {
		config.Filters = append(config.Filters, f)
	}
	return true
}

// Load filters from the disk
// And if any filter has zero ID, assign a new one
func loadFilters(array []filter) {
	for i := range array {
		filter := &array[i] // otherwise we're operating on a copy
		if filter.ID == 0 {
			filter.ID = assignUniqueFilterID()
		}

		if !filter.Enabled {
			// No need to load a filter that is not enabled
			continue
		}

		err := filter.load()
		if err != nil {
			log.Error("Couldn't load filter %d contents due to %s", filter.ID, err)
		}
	}
}

func deduplicateFilters() {
	// Deduplicate filters
	i := 0 // output index, used for deletion later
	urls := map[string]bool{}
	for _, filter := range config.Filters {
		if _, ok := urls[filter.URL]; !ok {
			// we didn't see it before, keep it
			urls[filter.URL] = true // remember the URL
			config.Filters[i] = filter
			i++
		}
	}

	// all entries we want to keep are at front, delete the rest
	config.Filters = config.Filters[:i]
}

// Set the next filter ID to max(filter.ID) + 1
func updateUniqueFilterID(filters []filter) {
	for _, filter := range filters {
		if nextFilterID < filter.ID {
			nextFilterID = filter.ID + 1
		}
	}
}

func assignUniqueFilterID() int64 {
	value := nextFilterID
	nextFilterID++
	return value
}

// Sets up a timer that will be checking for filters updates periodically
func periodicallyRefreshFilters() {
	const maxInterval = 1 * 60 * 60
	intval := 5 // use a dynamically increasing time interval
	for {
		isNetworkErr := false
		if config.DNS.FiltersUpdateIntervalHours != 0 && atomic.CompareAndSwapUint32(&refreshStatus, 0, 1) {
			refreshLock.Lock()
			_, isNetworkErr = refreshFiltersIfNecessary(FilterRefreshBlocklists | FilterRefreshAllowlists)
			refreshLock.Unlock()
			refreshStatus = 0
			if !isNetworkErr {
				intval = maxInterval
			}
		}

		if isNetworkErr {
			intval *= 2
			if intval > maxInterval {
				intval = maxInterval
			}
		}

		time.Sleep(time.Duration(intval) * time.Second)
	}
}

// Refresh filters
// flags: FilterRefresh*
// important:
//  TRUE: ignore the fact that we're currently updating the filters
func refreshFilters(flags int, important bool) (int, error) {
	set := atomic.CompareAndSwapUint32(&refreshStatus, 0, 1)
	if !important && !set {
		return 0, fmt.Errorf("Filters update procedure is already running")
	}

	refreshLock.Lock()
	nUpdated, _ := refreshFiltersIfNecessary(flags)
	refreshLock.Unlock()
	refreshStatus = 0
	return nUpdated, nil
}

func refreshFiltersArray(filters *[]filter, force bool) (int, []filter, []bool, bool) {
	var updateFilters []filter
	var updateFlags []bool // 'true' if filter data has changed

	now := time.Now()
	config.RLock()
	for i := range *filters {
		f := &(*filters)[i] // otherwise we will be operating on a copy

		if !f.Enabled {
			continue
		}

		expireTime := f.LastUpdated.Unix() + int64(config.DNS.FiltersUpdateIntervalHours)*60*60
		if !force && expireTime > now.Unix() {
			continue
		}

		var uf filter
		uf.ID = f.ID
		uf.URL = f.URL
		uf.Name = f.Name
		uf.checksum = f.checksum
		updateFilters = append(updateFilters, uf)
	}
	config.RUnlock()

	if len(updateFilters) == 0 {
		return 0, nil, nil, false
	}

	nfail := 0
	for i := range updateFilters {
		uf := &updateFilters[i]
		updated, err := uf.update()
		updateFlags = append(updateFlags, updated)
		if err != nil {
			nfail++
			log.Printf("Failed to update filter %s: %s\n", uf.URL, err)
			continue
		}
	}

	if nfail == len(updateFilters) {
		return 0, nil, nil, true
	}

	updateCount := 0
	for i := range updateFilters {
		uf := &updateFilters[i]
		updated := updateFlags[i]

		config.Lock()
		for k := range *filters {
			f := &(*filters)[k]
			if f.ID != uf.ID || f.URL != uf.URL {
				continue
			}
			f.LastUpdated = uf.LastUpdated
			if !updated {
				continue
			}

			log.Info("Updated filter #%d.  Rules: %d -> %d",
				f.ID, f.RulesCount, uf.RulesCount)
			f.Name = uf.Name
			f.RulesCount = uf.RulesCount
			f.checksum = uf.checksum
			updateCount++
		}
		config.Unlock()
	}

	return updateCount, updateFilters, updateFlags, false
}

const (
	FilterRefreshForce      = 1 // ignore last file modification date
	FilterRefreshAllowlists = 2 // update allow-lists
	FilterRefreshBlocklists = 4 // update block-lists
)

// Checks filters updates if necessary
// If force is true, it ignores the filter.LastUpdated field value
// flags: FilterRefresh*
//
// Algorithm:
// . Get the list of filters to be updated
// . For each filter run the download and checksum check operation
//  . Store downloaded data in a temporary file inside data/filters directory
// . For each filter:
//  . If filter data hasn't changed, just set new update time on file
//  . If filter data has changed:
//    . rename the temporary file (<temp> -> 1.txt)
//      Note that this method works only on UNIX.
//      On Windows we don't pass files to dnsfilter - we pass the whole data.
//  . Pass new filters to dnsfilter object - it analyzes new data while the old filters are still active
//  . dnsfilter activates new filters
//
// Return the number of updated filters
// Return TRUE - there was a network error and nothing could be updated
func refreshFiltersIfNecessary(flags int) (int, bool) {
	log.Debug("Filters: updating...")

	updateCount := 0
	var updateFilters []filter
	var updateFlags []bool
	netError := false
	netErrorW := false
	force := false
	if (flags & FilterRefreshForce) != 0 {
		force = true
	}
	if (flags & FilterRefreshBlocklists) != 0 {
		updateCount, updateFilters, updateFlags, netError = refreshFiltersArray(&config.Filters, force)
	}
	if (flags & FilterRefreshAllowlists) != 0 {
		updateCountW := 0
		var updateFiltersW []filter
		var updateFlagsW []bool
		updateCountW, updateFiltersW, updateFlagsW, netErrorW = refreshFiltersArray(&config.WhitelistFilters, force)
		updateCount += updateCountW
		updateFilters = append(updateFilters, updateFiltersW...)
		updateFlags = append(updateFlags, updateFlagsW...)
	}
	if netError && netErrorW {
		return 0, true
	}

	if updateCount != 0 {
		enableFilters(false)

		for i := range updateFilters {
			uf := &updateFilters[i]
			updated := updateFlags[i]
			if !updated {
				continue
			}
			_ = os.Remove(uf.Path() + ".old")
		}
	}

	log.Debug("Filters: update finished")
	return updateCount, false
}

// Allows printable UTF-8 text with CR, LF, TAB characters
func isPrintableText(data []byte) bool {
	for _, c := range data {
		if (c >= ' ' && c != 0x7f) || c == '\n' || c == '\r' || c == '\t' {
			continue
		}
		return false
	}
	return true
}

// A helper function that parses filter contents and returns a number of rules and a filter name (if there's any)
func parseFilterContents(f io.Reader) (int, uint32, string) {
	rulesCount := 0
	name := ""
	seenTitle := false
	r := bufio.NewReader(f)
	checksum := uint32(0)

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			break
		}

		checksum = crc32.Update(checksum, crc32.IEEETable, []byte(line))

		line = strings.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		if line[0] == '!' {
			m := filterTitleRegexp.FindAllStringSubmatch(line, -1)
			if len(m) > 0 && len(m[0]) >= 2 && !seenTitle {
				name = m[0][1]
				seenTitle = true
			}
		} else {
			rulesCount++
		}
	}

	return rulesCount, checksum, name
}

// Perform upgrade on a filter and update LastUpdated value
func (filter *filter) update() (bool, error) {
	b, err := filter.updateIntl()
	filter.LastUpdated = time.Now()
	if !b {
		e := os.Chtimes(filter.Path(), filter.LastUpdated, filter.LastUpdated)
		if e != nil {
			log.Error("os.Chtimes(): %v", e)
		}
	}
	return b, err
}

func (filter *filter) updateIntl() (bool, error) {
	log.Tracef("Downloading update for filter %d from %s", filter.ID, filter.URL)

	tmpfile, err := ioutil.TempFile(filepath.Join(Context.getDataDir(), filterDir), "")
	if err != nil {
		return false, err
	}
	defer func() {
		if tmpfile != nil {
			_ = tmpfile.Close()
			_ = os.Remove(tmpfile.Name())
		}
	}()

	resp, err := Context.client.Get(filter.URL)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		log.Printf("Couldn't request filter from URL %s, skipping: %s", filter.URL, err)
		return false, err
	}

	if resp.StatusCode != 200 {
		log.Printf("Got status code %d from URL %s, skipping", resp.StatusCode, filter.URL)
		return false, fmt.Errorf("got status code != 200: %d", resp.StatusCode)
	}

	htmlTest := true
	firstChunk := make([]byte, 4*1024)
	firstChunkLen := 0
	buf := make([]byte, 64*1024)
	total := 0
	for {
		n, err := resp.Body.Read(buf)
		total += n

		if htmlTest {
			// gather full buffer firstChunk and perform its data tests
			num := util.MinInt(n, len(firstChunk)-firstChunkLen)
			copied := copy(firstChunk[firstChunkLen:], buf[:num])
			firstChunkLen += copied

			if firstChunkLen == len(firstChunk) || err == io.EOF {
				if !isPrintableText(firstChunk) {
					return false, fmt.Errorf("Data contains non-printable characters")
				}

				s := strings.ToLower(string(firstChunk))
				if strings.Index(s, "<html") >= 0 ||
					strings.Index(s, "<!doctype") >= 0 {
					return false, fmt.Errorf("Data is HTML, not plain text")
				}

				htmlTest = false
				firstChunk = nil
			}
		}

		_, err2 := tmpfile.Write(buf[:n])
		if err2 != nil {
			return false, err2
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("Couldn't fetch filter contents from URL %s, skipping: %s", filter.URL, err)
			return false, err
		}
	}

	// Extract filter name and count number of rules
	_, _ = tmpfile.Seek(0, io.SeekStart)
	rulesCount, checksum, filterName := parseFilterContents(tmpfile)
	// Check if the filter has been really changed
	if filter.checksum == checksum {
		log.Tracef("Filter #%d at URL %s hasn't changed, not updating it", filter.ID, filter.URL)
		return false, nil
	}

	log.Printf("Filter %d has been updated: %d bytes, %d rules",
		filter.ID, total, rulesCount)
	if filterName != "" {
		filter.Name = filterName
	}
	filter.RulesCount = rulesCount
	filter.checksum = checksum
	filterFilePath := filter.Path()
	log.Printf("Saving filter %d contents to: %s", filter.ID, filterFilePath)
	err = os.Rename(tmpfile.Name(), filterFilePath)
	if err != nil {
		return false, err
	}
	tmpfile.Close()
	tmpfile = nil

	return true, nil
}

// loads filter contents from the file in dataDir
func (filter *filter) load() error {
	filterFilePath := filter.Path()
	log.Tracef("Loading filter %d contents to: %s", filter.ID, filterFilePath)

	if _, err := os.Stat(filterFilePath); os.IsNotExist(err) {
		// do nothing, file doesn't exist
		return err
	}

	f, err := os.Open(filterFilePath)
	if err != nil {
		return err
	}
	defer f.Close()
	st, _ := f.Stat()

	log.Tracef("File %s, id %d, length %d",
		filterFilePath, filter.ID, st.Size())
	rulesCount, checksum, _ := parseFilterContents(f)

	filter.RulesCount = rulesCount
	filter.checksum = checksum
	filter.LastUpdated = filter.LastTimeUpdated()

	return nil
}

// Clear filter rules
func (filter *filter) unload() {
	filter.RulesCount = 0
	filter.checksum = 0
}

// Path to the filter contents
func (filter *filter) Path() string {
	return filepath.Join(Context.getDataDir(), filterDir, strconv.FormatInt(filter.ID, 10)+".txt")
}

// LastTimeUpdated returns the time when the filter was last time updated
func (filter *filter) LastTimeUpdated() time.Time {
	filterFilePath := filter.Path()
	s, err := os.Stat(filterFilePath)
	if os.IsNotExist(err) {
		// if the filter file does not exist, return 0001-01-01
		return time.Time{}
	}

	if err != nil {
		// if the filter file does not exist, return 0001-01-01
		return time.Time{}
	}

	// filter file modified time
	return s.ModTime()
}

func enableFilters(async bool) {
	var filters []dnsfilter.Filter
	var whiteFilters []dnsfilter.Filter
	if config.DNS.FilteringEnabled {
		// convert array of filters

		userFilter := userFilter()
		f := dnsfilter.Filter{
			ID:   userFilter.ID,
			Data: userFilter.Data,
		}
		filters = append(filters, f)

		for _, filter := range config.Filters {
			if !filter.Enabled {
				continue
			}
			f := dnsfilter.Filter{
				ID:       filter.ID,
				FilePath: filter.Path(),
			}
			filters = append(filters, f)
		}
		for _, filter := range config.WhitelistFilters {
			if !filter.Enabled {
				continue
			}
			f := dnsfilter.Filter{
				ID:       filter.ID,
				FilePath: filter.Path(),
			}
			whiteFilters = append(whiteFilters, f)
		}
	}

	_ = Context.dnsFilter.SetFilters(filters, whiteFilters, async)
}
