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
	if err := godotenv.Load(); err != nil {
		fmt.Println("[x] Warning: No .env file found")
	}

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

	fmt.Print(logMessage)

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

// Perbaikan Logika Jam Kerja (Support lintas hari, misal 22:00 s/d 05:00)
func isActive() bool {
	now := time.Now().In(location)
	hour := now.Hour()

	if hourStart < hourEnd {
		// Jam kerja normal (misal 08:00 - 17:00)
		return hour >= hourStart && hour < hourEnd
	} else {
		// Jam kerja lintas hari (misal 22:00 - 05:00)
		return hour >= hourStart || hour < hourEnd
	}
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

	// Jika tidak aktif, hentikan eksekusi di sini
	if !active {
		return
	}

	if debugMode {
		writeLog("[DEBUG] Checking email")
	}

	c, err := client.DialTLS(fmt.Sprintf("%s:%d", imapHost, imapPort), nil)
	if err != nil {
		writeLog(fmt.Sprintf("[x] IMAP Connection Error: %v", err))
		return
	}
	defer c.Logout()

	if err := c.Login(emailUser, emailPass); err != nil {
		writeLog(fmt.Sprintf("[x] IMAP Login Error: %v", err))
		return
	}

	_, err = c.Select("INBOX", false)
	if err != nil {
		writeLog(fmt.Sprintf("[x] IMAP Select Error: %v", err))
		return
	}

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

	// Safety: Pastikan email selalu ditandai 'Seen' di akhir
	defer func() {
		markAsSeen(c, msg.Uid)
	}()

	writeLog(fmt.Sprintf("Email UID: %d | Subject: %s", msg.Uid, msg.Envelope.Subject))

	// 1. Ambil Sender
	var fromEmail, fromName string
	if len(msg.Envelope.From) > 0 {
		fromEmail = strings.ToLower(msg.Envelope.From[0].MailboxName + "@" + msg.Envelope.From[0].HostName)
		fromName = msg.Envelope.From[0].PersonalName
	}

	subject := msg.Envelope.Subject

	// Cek apakah email dari akun sendiri
	isFromSelf := strings.ToLower(fromEmail) == strings.ToLower(emailUser)

	// 2. Loop Prevention: Abaikan jika ini adalah balasan (Re:) dari kita sendiri
	if isFromSelf && strings.HasPrefix(strings.ToLower(subject), "re:") {
		writeLog("  [!] IGNORED: Our own auto-reply (loop prevention)")
		return
	}

	// 3. Filter Auto-mailer (kecuali jika dari diri sendiri, kita asumsikan itu form notification)
	if !isFromSelf {
		if strings.Contains(fromEmail, "no-reply") ||
			strings.Contains(fromEmail, "noreply") ||
			strings.Contains(fromEmail, "mailer-daemon") ||
			strings.Contains(strings.ToLower(subject), "auto") {
			writeLog("  [!] IGNORED: Auto-mailer detected")
			return
		}

		ignoreDomains := []string{"@stripe.com", "@amazon.com.au"}
		for _, domain := range ignoreDomains {
			if strings.HasSuffix(fromEmail, domain) {
				writeLog(fmt.Sprintf("  [!] IGNORED: Domain in ignore list (%s)", fromEmail))
				return
			}
		}
	}

	// 4. Baca Body Email (Wajib untuk ekstraksi Fluent Form)
	r := msg.GetBody(section)
	if r == nil {
		writeLog("  [!] ERROR: Could not get email body")
		return
	}

	mr, err := mail.CreateReader(r)
	if err != nil {
		writeLog(fmt.Sprintf("  [!] ERROR: Could not parse email: %v", err))
		return
	}

	var bodyBuilder strings.Builder
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
			bodyBuilder.Write(b)
		case *mail.AttachmentHeader:
			_ = h
		}
	}
	emailBody := bodyBuilder.String()

	// 5. Tentukan Target Balasan
	targetEmail := ""
	targetName := "there"

	if isFromSelf {
		writeLog("  [*] Email from SELF detected. Analyzing as Form Notification...")

		// A. Prioritas 1: Cek Header Reply-To
		// Fluent Form biasanya menaruh email pelanggan di header Reply-To
		if len(msg.Envelope.ReplyTo) > 0 {
			replyToEmail := strings.ToLower(msg.Envelope.ReplyTo[0].MailboxName + "@" + msg.Envelope.ReplyTo[0].HostName)
			// Pastikan Reply-To bukan diri sendiri
			if replyToEmail != strings.ToLower(emailUser) {
				targetEmail = replyToEmail
				targetName = msg.Envelope.ReplyTo[0].PersonalName
				writeLog(fmt.Sprintf("  [v] Found customer via Reply-To: %s", targetEmail))
			}
		}

		// B. Prioritas 2: Regex Body HTML (Jika Reply-To gagal atau masih diri sendiri)
		if targetEmail == "" {
			// Regex mencari pola tabel HTML standard Fluent Forms
			// Mencari: <td> email@address.com </td> setelah header Email
			emailRegex := regexp.MustCompile(`(?i)<th[^>]*>\s*<strong[^>]*>\s*Email\s*</strong>\s*</th>[\s\S]*?<td[^>]*>\s*([^\s<]+@[^\s<]+)\s*</td>`)
			if matches := emailRegex.FindStringSubmatch(emailBody); len(matches) > 1 {
				targetEmail = strings.TrimSpace(matches[1])
			}

			// Mencari Nama
			nameRegex := regexp.MustCompile(`(?i)<th[^>]*>\s*<strong[^>]*>\s*Full Name\s*</strong>\s*</th>[\s\S]*?<td[^>]*>\s*([^<]+?)\s*</td>`)
			if matches := nameRegex.FindStringSubmatch(emailBody); len(matches) > 1 {
				targetName = strings.TrimSpace(matches[1])
			}

			if targetEmail != "" {
				writeLog(fmt.Sprintf("  [v] Extracted customer via Body Parsing: %s", targetEmail))
			}
		}

		// C. Jika Gagal Ekstraksi
		if targetEmail == "" {
			writeLog("  [!] IGNORED: From self, but failed to extract Customer Email from body/headers.")
			return // STOP. Jangan balas ke diri sendiri.
		}

	} else {
		// Email Normal (Bukan dari diri sendiri)
		targetEmail = fromEmail
		targetName = fromName
	}

	if targetName == "" {
		targetName = "there"
	}

	// 6. Kirim Auto Reply
	if err := sendAutoReply(targetEmail, targetName, msg.Envelope.MessageId); err != nil {
		writeLog(fmt.Sprintf("  [!] ERROR sending auto-reply to %s: %v", targetEmail, err))
		return
	}

	writeLog(fmt.Sprintf("  [v] Auto-reply sent successfully to: %s", targetEmail))

	// Opsional: Tandai sebagai dijawab di server
	markAsAnswered(c, msg.Uid)
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
	if err := c.UidStore(seqset, item, flags, nil); err != nil {
		writeLog(fmt.Sprintf("  [x] Failed to mark UID %d as seen: %v", uid, err))
	}
}

func markAsAnswered(c *client.Client, uid uint32) {
	seqset := new(imap.SeqSet)
	seqset.AddNum(uid)
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	flags := []interface{}{imap.AnsweredFlag}
	if err := c.UidStore(seqset, item, flags, nil); err != nil {
		writeLog(fmt.Sprintf("  [x] Failed to mark UID %d as answered: %v", uid, err))
	}
}

func main() {
	writeLog("===========================================")
	writeLog("GasPro Email Auto-Reply Service Started")
	writeLog("===========================================")

	mode := "PRODUCTION"
	if debugMode {
		mode = "DEBUG"
	}
	writeLog(fmt.Sprintf("Mode: %s", mode))

	checkInterval := prodTimeCheck
	if debugMode {
		checkInterval = debugTimeCheck
	}

	writeLog(fmt.Sprintf("Check interval: %d seconds", checkInterval))
	writeLog(fmt.Sprintf("Timezone: %s", timezone))
	writeLog(fmt.Sprintf("Active hours: %d:00 - %d:00 WIB", hourStart, hourEnd))
	writeLog("===========================================\n")

	// Jalankan sekali saat startup
	autoReply()

	ticker := time.NewTicker(time.Duration(checkInterval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		autoReply()
	}
}
