// This is for accessing The Arkansas Department of Education Cognos system.
// it might also work for other Cognos installations. It can list directories.
// and run/download reports (that have already been built) syncronously to CSV strings.
// It does not support anything other than default parameters, so save default parameters
// or build reports that don't have parameters. Basically everything panics on failure.
// I use a helper function called Try() to handle these pannics (http://github.com/9072997/jgh).
// This library would not have been possible without the code generously open sourced by
// Scott Organ (https://github.com/scottorgan/cognosant).
package cognos

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/antchfx/htmlquery"

	"github.com/9072997/jgh"
	"github.com/Azure/go-ntlmssp"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/sync/semaphore"
)

type CognosInstance struct {
	User         string
	Pass         string
	URL          string
	DSN          string
	RetryDelay   uint
	RetryCount   int
	client       http.Client
	httpLockPool *semaphore.Weighted
}

type FolderEntryType uint

const (
	Folder FolderEntryType = iota
	Report FolderEntryType = iota
)

// FolderEntry represents either a folder or a report
// (anything that can be in a folder)
type FolderEntry struct {
	// compare this with the constants Folder or Type
	Type FolderEntryType `json:"type"`
	ID   string          `json:"id"`
}

// MarshalJSON marshals a field that is basically an enum.
// in case you want to convert directoryEntries to json
func (t FolderEntryType) MarshalJSON() ([]byte, error) {
	if t == Folder {
		return []byte(`"folder"`), nil
	} else if t == Report {
		return []byte(`"report"`), nil
	} else {
		return nil, &json.UnsupportedValueError{
			Value: reflect.ValueOf(t),
			Str:   "folderEntryType is a nominal number, but the provided value was unknown",
		}
	}
}

// MakeInstance creates a new cognos object.
// user is the user used to connect to Cognos (ex: APSCN\0401jpenn).
// This value also changes which "my folders" folder ~ points to.
// url is the base URL of the cognos server (ex: https://adecognos.arkansas.gov).
// I don't totally know what dsn is, but mine is bentonvisms.
// If you open cognos in eschool and view source, you can see this value in the URL for the iframe.
// There is a diffrent one for eschool and e-finance.
// retryDelay is the number of seconds before a failed request will be retried.
// It is also the polling interval when waiting for a report to finish.
// retryCount is the number of times a failed request will be retried.
// Polling unfinished reports is unaffected by this.
// httpTimeout is the number seconds before giving up on a Cognos HTTP request.
// concurentRequests limits the maximum number of requests going at once.
func MakeInstance(user, pass, url, dsn string, retryDelay uint, retryCount int, httpTimeout uint, concurentRequests uint) (c CognosInstance) {
	c = CognosInstance{
		User:         user,
		Pass:         pass,
		URL:          url,
		DSN:          dsn,
		RetryDelay:   retryDelay,
		RetryCount:   retryCount,
		httpLockPool: semaphore.NewWeighted(int64(concurentRequests)),
	}

	// make a new cookie jar
	// (cookie jars are threadsafe)
	jar, err := cookiejar.New(
		&cookiejar.Options{
			PublicSuffixList: publicsuffix.List,
		},
	)
	jgh.PanicOnErr(err)

	// make a httpClient that uses the cookie jar and supports NTLM auth
	c.client = http.Client{
		Transport: ntlmssp.Negotiator{
			RoundTripper: &http.Transport{},
		},
		Jar:     jar,
		Timeout: time.Duration(httpTimeout) * time.Second,
	}

	return
}

// loginLink returns the link that you must hit first to get cookies
// that will let you access the rest of cognos
func (c CognosInstance) loginLink() string {
	return "/ibmcognos/cgi-bin/cognos.cgi" +
		"?dsn=" + c.DSN +
		"&CAMNamespace=esp" +
		"&b_action=xts.run" +
		"&m=portal/cc.xts" +
		"&gohome="
}

// folderLinkFromID returns a link for use with Request() for a given folderID
func folderLinkFromID(id string) string {
	return "/ibmcognos/cgi-bin/cognos.cgi" +
		"?b_action=xts.run" +
		"&m=portal/cc.xts" +
		"&m_folder=" + id
}

// folderIDFromLink tries to pull the folderID out of a link.
// This may panic if the link does not point to a cognos folder
func folderIDFromLink(link string) string {
	pattern := regexp.MustCompile(`&m_folder=([0-9a-zA-Z-]+)`)
	matchParts := pattern.FindStringSubmatch(link)
	if len(matchParts) < 2 {
		panic("Unable to find folder ID from link: " + link)
	}
	return matchParts[1]
}

// reportLinkFromID returns a link for use with Request() for a given reportID
func reportLinkFromID(id string) string {
	return "/ibmcognos/cgi-bin/cognos.cgi" +
		"?b_action=cognosViewer" +
		"&ui.action=run" +
		"&ui.object=" + url.QueryEscape(id) +
		"&run.outputFormat=CSV" +
		"&run.prompt=false"
}

// BUG(jon): dosen't support "my folders" unless you specify the username
// BUG(jon): this is unused and untested, but seems like it might be usefull one day (tm)
func reportIDFromPath(path []string) (id string) {
	// check that the path is long enough
	if len(path) < 2 {
		panic(`A report path must contain at least a user (or "public") and a report name`)
	}

	// the first path element should be either a username
	// (ex: 0401jpenn) or "public"
	if path[0] == "public" {
		// this one has a leading slash
		id = "/content"
	} else {
		// no leading slash
		id = `CAMID("esp:a:` + cognosEscape(path[0]) + `")`
	}

	// The first element is user. The last is report. Everything
	// inbetween is folders. We handle folders here.
	folderNames := path[1 : len(path)-1]
	for _, folderName := range folderNames {
		id += "/folder[@name='" + cognosEscape(folderName) + "']"
	}

	// the last path element is report
	reportName := path[len(path)-1]
	id += "/report[@name='" + cognosEscape(reportName) + "']"

	return id
}

// FolderEntryFromPath returns a folderEntry object representing whatever is
// at path. Path is a sloce of strings. The first string should be either "public"
// or "~" for public folders or my folders. Each string after that should represent
// the name of a folder. The last string may be the name of a report or a folder.
// BUG(jon): dosen't support "my folders" by username (only ~)
func (c CognosInstance) FolderEntryFromPath(path []string) FolderEntry {
	if len(path) == 0 {
		panic("Cannot get folder entry for empty path")
	}

	currentEntry := FolderEntry{
		Type: Folder,
	}
	if path[0] == "public" {
		currentEntry.ID, _ = c.findFolderRoots()
	} else if path[0] == "~" {
		_, currentEntry.ID = c.findFolderRoots()
	} else {
		panic("Invalid root folder " + path[0])
	}

	// skip the first component in the path. We handled it already.
	for i, pathComponent := range path[1:] {
		entries := c.LsFolder(currentEntry.ID)

		// look at the folder entry named after our next path component
		// panic if it dosen't exist
		nextEntry, exists := entries[pathComponent]
		if !exists {
			panic("Could not find folder entry " + pathComponent)
		}

		// panic if we find a report in the middle of a path
		isLastComponent := len(path)-2 == i
		if nextEntry.Type == Report && !isLastComponent {
			panic(pathComponent + " is a report but it is in the middle of a path")
		}

		currentEntry = nextEntry
	}

	return currentEntry
}

// Request makes a HTTP GET request to the link (not including hostname)
// provided via the "link" parameter. The response body is returned as a string.
// Any errors (including a non-200 response) will cause this function to panic.
func (c CognosInstance) Request(method string, link string, reqBody string) (respBody string) {
	// limit concurent requests
	// background means don't give up waiting for lock
	err := c.httpLockPool.Acquire(context.Background(), 1)
	jgh.PanicOnErr(err)
	defer c.httpLockPool.Release(1)

	success, _ := jgh.Try(int(c.RetryDelay), c.RetryCount, true, "", func() bool {
		// make an io.reader if we have post data
		var reqBodyReader io.Reader
		if len(reqBody) > 0 {
			reqBodyReader = strings.NewReader(reqBody)
		} else {
			reqBodyReader = nil
		}

		// set up and send a GET request (no body)
		req, err := http.NewRequest(method, c.URL+link, reqBodyReader)
		jgh.PanicOnErr(err)
		req.SetBasicAuth(c.User, c.Pass)
		resp, err := c.client.Do(req)
		jgh.PanicOnErr(err)
		defer resp.Body.Close()

		// check HTTP response code
		if resp.StatusCode == 401 {
			// since cognos gives us random 401s in normal operation we don't panic.
			// it produces a lot of ugly debug output.
			log.Println("Invalid Password. Cognos also returns this error randomly sometimes?")
			// this still indicates failure and will trigger a retry
			return false
		} else if resp.StatusCode != 200 {
			panic("Error from Cognos while logging on: " + resp.Status)
		}

		respBody = jgh.ReadAll(resp.Body)
		return true
	})
	if !success {
		panic("Cognos request to " + link + " failed.")
	}
	return respBody
}

func (c *CognosInstance) findFolderRoots() (publicFolderID string, myFolderID string) {
	respHTML := c.Request("GET", c.loginLink(), "")

	// find the public folder ID from a regex.
	pattern := regexp.MustCompile(`var g_PS_PFRootId = "([0-9a-zA-Z-]+)";`)
	matchParts := pattern.FindStringSubmatch(respHTML)
	if len(matchParts) < 2 {
		panic("Unable to find Cognos public root folder ID")
	}
	publicFolderID = matchParts[1]

	// the same thing for "My Folder"
	pattern = regexp.MustCompile(`var g_PS_MFRootId = "([0-9a-zA-Z-]+)";`)
	matchParts = pattern.FindStringSubmatch(respHTML)
	if len(matchParts) < 2 {
		panic("Unable to find Cognos \"my folder\" ID")
	}
	myFolderID = matchParts[1]

	return
}

// BUG(jon): This just panics on questionable charicters.
// eventuially it would be nice to actuially escape these.
// BUG(jon): this is unused
func cognosEscape(s string) string {
	if strings.ContainsAny(s, `"':\`) {
		panic("The value " + s + " contains an illegal charicter")
	}
	return s
}

// LsFolder returnes a map of folder/report names to objects. Each object
// represents a folder entry. Each entry has a type (folder or report)
// and an ID
func (c CognosInstance) LsFolder(id string) map[string]FolderEntry {
	respHTML := c.Request("GET", folderLinkFromID(id), "")

	// get all links in the main table. These corrispond to folder entries.
	docTree, err := htmlquery.Parse(strings.NewReader(respHTML))
	jgh.PanicOnErr(err)
	query := `//td[@class="tableText"]/a]`
	elements := htmlquery.Find(docTree, query)

	// turn our html elements into a map of folder entries
	// keyed by name
	entries := make(map[string]FolderEntry)
	for _, element := range elements {
		linkText := htmlquery.InnerText(element)
		link := htmlquery.SelectAttr(element, "href")

		// create an entry variable with only the name for now.
		// we will fill in the other attributes
		var entry FolderEntry

		// Get the folder ID. This might not be a folder though,
		// so don't panic if it isn't
		foundID, _ := jgh.Try(0, 1, false, "", func() bool {
			entry.ID = folderIDFromLink(link)
			// if we made it this far, it's a folder
			entry.Type = Folder

			return true
		})

		// if we haven't found the ID yet, try assuming it's a report
		if !foundID {
			foundID, _ = jgh.Try(0, 1, false, "", func() bool {
				// parse url so we can get reliable query params
				urlObj, err := url.Parse(link)
				jgh.PanicOnErr(err)
				queryParams, err := url.ParseQuery(urlObj.RawQuery)
				jgh.PanicOnErr(err)

				// fill out our entry struct. This could fail if our link dosen't
				// have a "ui.object"
				entry.ID = queryParams["ui.object"][0]
				entry.Type = Report

				return true
			})
		}

		// if we still haven't found the ID, panic
		if !foundID {
			panic("Can not parse " + linkText + " as a folder or as a report")
		}

		entries[linkText] = entry
	}

	return entries
}
