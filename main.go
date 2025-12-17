package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"
	"github.com/joho/godotenv"
	"gopkg.in/gomail.v2"
)

const (
	logFile  = "logs.txt"
	timezone = "Asia/Jakarta"
)

var (
	emailUser      string
	emailPass      string
	smtpHost       string
	smtpPort       int
	imapHost       string
	imapPort       int
	hourStart      int
	hourEnd        int
	debugTimeCheck int
	prodTimeCheck  int
	debugMode      bool
	location       *time.Location
	showRun        bool = true
	showInactive   bool = true
)

func init() {
	// Load .env file
	if err := godotenv.Load(); err != nil {
		writeLog("[x] No .env file found")
	}

	// Load configuration
	emailUser = os.Getenv("EMAIL_USER")
	emailPass = os.Getenv("EMAIL_PASS")
	smtpHost = os.Getenv("SMTP_HOST")
	smtpPort = getEnvAsInt("SMTP_PORT", 465)
	imapHost = os.Getenv("IMAP_HOST")
	imapPort = getEnvAsInt("IMAP_PORT", 993)
	hourStart = getEnvAsInt("HOUR_START", 0)
	hourEnd = getEnvAsInt("HOUR_END", 24)
	debugTimeCheck = getEnvAsInt("DEBUG_TIME_CHECK", 30)
	prodTimeCheck = getEnvAsInt("PROD_TIME_CHECK", 60)
	debugMode = os.Getenv("DEBUG_MODE") == "true"

	// Load timezone
	var err error
	location, err = time.LoadLocation(timezone)
	if err != nil {
		log.Printf("Error loading timezone, using UTC: %v", err)
		location = time.UTC
	}
}

func getEnvAsInt(key string, defaultVal int) int {
	valStr := os.Getenv(key)
	if val, err := strconv.Atoi(valStr); err == nil {
		return val
	}
	return defaultVal
}

func writeLog(message string) {
	timestamp := time.Now().In(location).Format("2006-01-02 15:04:05")
	logMessage := fmt.Sprintf("[%s] %s\n", timestamp, message)

	// Write to console
	fmt.Print(logMessage)

	// Write to file
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[x] Error opening log file: %v", err)
		return
	}
	defer f.Close()

	if _, err := f.WriteString(logMessage); err != nil {
		log.Printf("[x] Error writing to log file: %v", err)
	}
}

func isActive() bool {
	now := time.Now().In(location)
	hour := now.Hour()

	return hour >= hourStart && hour < hourEnd
}

func autoReply() {
	active := isActive()

	if active && showRun {
		showRun = false
		showInactive = true
		writeLog("[v] Auto-reply now running")
	}

	if !active && showInactive {
		showRun = true
		showInactive = false
		writeLog("[*] Auto-reply inactive (outside active hours)")
		return
	}

	if debugMode {
		writeLog("[DEBUG] Checking email")
	}

	// Connect to IMAP server
	c, err := client.DialTLS(fmt.Sprintf("%s:%d", imapHost, imapPort), nil)
	if err != nil {
		writeLog(fmt.Sprintf("[x] IMAP Connection Error: %v", err))
		return
	}
	defer c.Logout()

	// Login
	if err := c.Login(emailUser, emailPass); err != nil {
		writeLog(fmt.Sprintf("[x] IMAP Login Error: %v", err))
		return
	}

	// Select INBOX
	_, err = c.Select("INBOX", false)
	if err != nil {
		writeLog(fmt.Sprintf("[x] IMAP Select Error: %v", err))
		return
	}

	// Search for unseen and unanswered emails
	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{imap.SeenFlag, imap.AnsweredFlag}

	uids, err := c.Search(criteria)
	if err != nil {
		writeLog(fmt.Sprintf("[x] IMAP Search Error: %v", err))
		return
	}

	if len(uids) == 0 {
		if debugMode {
			writeLog("[*] No new emails to process")
		}
		return
	}

	writeLog(fmt.Sprintf("[v] Found %d new email(s) to process", len(uids)))

	seqset := new(imap.SeqSet)
	seqset.AddNum(uids...)

	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)

	section := &imap.BodySectionName{}
	items := []imap.FetchItem{section.FetchItem(), imap.FetchEnvelope, imap.FetchUid}

	go func() {
		done <- c.Fetch(seqset, items, messages)
	}()

	for msg := range messages {
		processEmail(c, msg, section)
	}

	if err := <-done; err != nil {
		writeLog(fmt.Sprintf("[x] Fetch Error: %v", err))
	}

	writeLog("--- Auto-reply cycle completed ---")
}

func processEmail(c *client.Client, msg *imap.Message, section *imap.BodySectionName) {
	if msg == nil || msg.Envelope == nil {
		return
	}

	writeLog(fmt.Sprintf("Email UID: %d", msg.Uid))

	// Get email body
	r := msg.GetBody(section)
	if r == nil {
		writeLog("  [!] ERROR: Could not get email body")
		return
	}

	// Parse email
	mr, err := mail.CreateReader(r)
	if err != nil {
		writeLog(fmt.Sprintf("  [!] ERROR: Could not parse email: %v", err))
		return
	}

	var fromEmail, fromName string
	if len(msg.Envelope.From) > 0 {
		fromEmail = strings.ToLower(msg.Envelope.From[0].MailboxName + "@" + msg.Envelope.From[0].HostName)
		fromName = msg.Envelope.From[0].PersonalName
	}

	subject := msg.Envelope.Subject

	// Check if from self
	isFromSelf := strings.ToLower(fromEmail) == strings.ToLower(emailUser)

	// Ignore own auto-replies (loop prevention)
	if isFromSelf && strings.HasPrefix(strings.ToLower(subject), "re:") {
		writeLog("  [!] IGNORED: Our own auto-reply (loop prevention)")
		markAsSeen(c, msg.Uid)
		return
	}

	// Ignore auto-mailers
	if strings.Contains(fromEmail, "no-reply") ||
		strings.Contains(fromEmail, "noreply") ||
		strings.Contains(fromEmail, "mailer-daemon") ||
		strings.Contains(strings.ToLower(subject), "auto") {
		writeLog("  [!] IGNORED: Auto-mailer detected")
		markAsSeen(c, msg.Uid)
		return
	}

	// Ignore specific domains
	ignoreDomains := []string{"@stripe.com", "@amazon.com.au"}
	for _, domain := range ignoreDomains {
		if strings.HasSuffix(fromEmail, domain) {
			writeLog(fmt.Sprintf("  [!] IGNORED: Domain in ignore list (%s)", fromEmail))
			markAsSeen(c, msg.Uid)
			return
		}
	}

	// Read email body
	var emailBody string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}

		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			b, _ := io.ReadAll(p.Body)
			emailBody += string(b)
		case *mail.AttachmentHeader:
			// Skip attachments
			_ = h
		}
	}

	targetEmail := ""
	targetName := "there"
	isFluentForm := isFromSelf

	if isFluentForm {
		// Check Reply-To first
		if len(msg.Envelope.ReplyTo) > 0 {
			targetEmail = strings.ToLower(msg.Envelope.ReplyTo[0].MailboxName + "@" + msg.Envelope.ReplyTo[0].HostName)
			targetName = msg.Envelope.ReplyTo[0].PersonalName
			if targetName == "" {
				targetName = "there"
			}
		}

		// Extract from body if no Reply-To
		if targetEmail == "" && emailBody != "" {
			emailRegex := regexp.MustCompile(`(?i)<th[^>]*>\s*<strong[^>]*>\s*Email\s*</strong>\s*</th>[\s\S]*?<td[^>]*>\s*([^\s<]+@[^\s<]+)\s*</td>`)
			if matches := emailRegex.FindStringSubmatch(emailBody); len(matches) > 1 {
				targetEmail = strings.TrimSpace(matches[1])
			}

			nameRegex := regexp.MustCompile(`(?i)<th[^>]*>\s*<strong[^>]*>\s*Full Name\s*</strong>\s*</th>[\s\S]*?<td[^>]*>\s*([^<]+?)\s*</td>`)
			if matches := nameRegex.FindStringSubmatch(emailBody); len(matches) > 1 {
				targetName = strings.TrimSpace(matches[1])
			}

			writeLog(fmt.Sprintf("  [!] Target email: %s", targetEmail))
			writeLog(fmt.Sprintf("  [!] Target name: %s", targetName))
		}

		if targetEmail == "" {
			writeLog("  [!] IGNORED: Cannot extract customer email from Fluent Form")
			markAsSeen(c, msg.Uid)
			return
		}

		writeLog(fmt.Sprintf("  [!] Fluent Form - replying to: %s <%s>", targetName, targetEmail))
	} else {
		targetEmail = fromEmail
		if fromName != "" {
			targetName = fromName
		}
	}

	// Send auto-reply
	if err := sendAutoReply(targetEmail, targetName, msg.Envelope.MessageId); err != nil {
		writeLog(fmt.Sprintf("  [!] ERROR sending auto-reply: %v", err))
		return
	}

	writeLog(fmt.Sprintf("  [v] Auto-reply sent successfully to: %s", targetEmail))

	// Mark as seen and answered
	markAsSeenAndAnswered(c, msg.Uid)
	writeLog("  [v] Email marked as Seen and Answered")
}

func sendAutoReply(to, name, messageID string) error {
	m := gomail.NewMessage()
	m.SetHeader("From", fmt.Sprintf("GasPro Detection <%s>", emailUser))
	m.SetHeader("To", fmt.Sprintf("%s <%s>", name, to))
	m.SetHeader("Subject", "Re: We'll Reply Soon As Possible")

	body := fmt.Sprintf(`Dear %s,

Thank you for contacting GasPro Detection.

Your message has been received and is currently being reviewed by our team. One of our representatives will get back to you as soon as possible.

Kind regards,
GasPro Detection Team`, name)

	m.SetBody("text/plain", body)

	if messageID != "" {
		m.SetHeader("In-Reply-To", messageID)
		m.SetHeader("References", messageID)
	}

	d := gomail.NewDialer(smtpHost, smtpPort, emailUser, emailPass)

	return d.DialAndSend(m)
}

func markAsSeen(c *client.Client, uid uint32) {
	seqset := new(imap.SeqSet)
	seqset.AddNum(uid)
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	flags := []interface{}{imap.SeenFlag}
	c.UidStore(seqset, item, flags, nil)
}

func markAsSeenAndAnswered(c *client.Client, uid uint32) {
	seqset := new(imap.SeqSet)
	seqset.AddNum(uid)
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	flags := []interface{}{imap.SeenFlag, imap.AnsweredFlag}
	c.UidStore(seqset, item, flags, nil)
}

func main() {
	// Initialize log
	writeLog("===========================================")
	writeLog("GasPro Email Auto-Reply Service Started")
	writeLog("===========================================")
	if debugMode {
		writeLog("Mode: DEBUG")
	} else {
		writeLog("Mode: PRODUCTION")
	}

	checkInterval := prodTimeCheck
	if debugMode {
		checkInterval = debugTimeCheck
	}

	writeLog(fmt.Sprintf("Check interval: %d seconds", checkInterval))
	writeLog(fmt.Sprintf("Timezone: %s", timezone))
	writeLog(fmt.Sprintf("Active hours: %d:00 - %d:00 WIB", hourStart, hourEnd))
	writeLog("===========================================\n")

	autoReply()

	// Set up ticker for periodic checks
	ticker := time.NewTicker(time.Duration(checkInterval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		autoReply()
	}
}
