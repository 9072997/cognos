package cognos

import (
	"net/url"
	"regexp"
	"strings"
	"time"
)

// DownloadReportCSV returns a string containing CSV data for a cognos report.
// This function triggers the execution of the report, and may take a while
// to return.
func (c CognosInstance) DownloadReportCSV(id string) string {
	respHTML := c.Request("GET", reportLinkFromID(id), "")

	// if the report isn't finished we need to poll to see when it is
	if strings.Contains(respHTML, `"m_sStatus": "working"`) {
		// when we re-check if the report is done we need to send along some post
		// data to identify the report. The list here is stolen from scottorgan
		valuesToSend := make(url.Values)
		valuesToSend.Set("b_action", findJSONValueInPage(respHTML, "b_action"))
		valuesToSend.Set("cv.actionState", findJSONValueInPage(respHTML, "m_sActionState"))
		valuesToSend.Set("cv.catchLogOnFault", "true")
		valuesToSend.Set("cv.id", findJSONValueInPage(respHTML, "cv.id"))
		valuesToSend.Set("cv.objectPermissions", findJSONValueInPage(respHTML, "cv.objectPermissions"))
		valuesToSend.Set("cv.responseFormat", "data")
		valuesToSend.Set("cv.showFaultPage", "true")
		valuesToSend.Set("executionParameters", findJSONValueInPage(respHTML, "m_sParameters"))
		valuesToSend.Set("m_tracking", findJSONValueInPage(respHTML, "m_sTracking"))
		valuesToSend.Set("ui.action", "wait")
		valuesToSend.Set("ui.cafcontextid", findJSONValueInPage(respHTML, "m_sCAFContext"))
		valuesToSend.Set("ui.conversation", findJSONValueInPage(respHTML, "m_sConversation"))
		valuesToSend.Set("ui.object", findJSONValueInPage(respHTML, "ui.object"))
		valuesToSend.Set("ui.objectClass", findJSONValueInPage(respHTML, "ui.objectClass"))
		valuesToSend.Set("ui.primaryAction", findJSONValueInPage(respHTML, "ui.primaryAction"))
		postData := valuesToSend.Encode()

		// if either of these strings is present, the report is not finished
		wStr1 := `"m_sStatus": "working"`
		wStr2 := `&quot;m_sStatus&quot;: &quot;stillWorking&quot;`
		// loop until neither string is present
		for strings.Contains(respHTML, wStr1) || strings.Contains(respHTML, wStr2) {
			time.Sleep(time.Second * time.Duration(c.RetryDelay))
			respHTML = c.Request("POST", "/ibmcognos/cgi-bin/cognos.cgi", postData)
		}

	}

	downloadLinkRegex := regexp.MustCompile(`var sURL = '([^']+)';`)
	if matchParts := downloadLinkRegex.FindStringSubmatch(respHTML); len(matchParts) > 0 {
		// ^ if a match is found for downloadLinkRegex ^
		downloadUrl := matchParts[1]

		// download the report
		csv := c.Request("GET", downloadUrl, "")
		return csv
	} else if strings.Contains(respHTML, `"m_sStatus": "prompting"`) {
		panic("the report prompted for additional information")
	} else {
		panic("Cognos returned a page we could not understand when attempting to run the report")
	}
}

// stolen from scottorgan. This is where it gets messy
// BUG(jon): searchKey is not escaped
func findJSONValueInPage(html string, key string) string {
	// build a regex that searches for
	// "key": "valuable-data"
	pattern := regexp.MustCompile(`"` + key + `": "(.*?)"`)
	matchParts := pattern.FindStringSubmatch(html)

	// panic if we didn't find a match
	if len(matchParts) == 0 {
		panic("Could not find JSON value " + key + " in page")
	}

	// return only the value (not the whole match)
	return matchParts[1]
}
