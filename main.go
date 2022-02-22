package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime/multipart"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const Name = "xmailgun"
const Version = "1.0.0"
const UserAgent string = Name + "/" + Version

type Config struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Type     string `json:"type"`
}

type Mail struct {
	Sender      string
	To          []string
	Cc          []string
	Bcc         []string
	ReplyTo     string
	Subject     string
	Text        string
	Charset     string
	Attachments []Attachment
}

type Attachment struct {
	Path    string `json:"path"`
	Type    string `json:"type"`
	Charset string `json:"charset"`
}

type Variable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Recipient struct {
	Realname    string       `json:"realname"`
	Email       string       `json:"email"`
	Url         string       `json:"url"`
	Attachments []Attachment `json:"attachments"`
	Variables   []Variable   `json:"variables"`
}

type Task struct {
	Name          string       `json:"name"`
	Subject       string       `json:"subject"`
	Sender        string       `json:"sender"`
	ReplyTo       string       `json:"replyto"`
	Cc            []string     `json:"cc"`
	Bcc           []string     `json:"bcc"`
	Bodytemplate  string       `json:"bodytemplate"`
	Charset       string       `json:"charset"`
	Recipientfile string       `json:"recipientfile"`
	Attachments   []Attachment `json:"attachments"`
}

var (
	err error

	DebugLogger *log.Logger
	ErrorLogger *log.Logger

	smtpConfigFile = ""
	taskFile       = ""
	dryRun         = false

	countdown = 30
	cooldown  = 30
)

func init() {
	flag.StringVar(&smtpConfigFile, "config", "", "configuration file for smtp connection (json)")
	flag.StringVar(&taskFile, "task", "", "task file (json)")
	flag.BoolVar(&dryRun, "dryrun", false, "do not actually send mails")

	file, err := os.OpenFile(Name+".log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		ErrorLogger.Fatal(err)
	}

	mw := io.MultiWriter(os.Stdout, file)

	DebugLogger = log.New(file, "Debug: ", log.Ldate|log.Ltime|log.Lshortfile)
	ErrorLogger = log.New(mw, "Error: ", log.Ldate|log.Ltime|log.Lshortfile)

}

func getSmtpConfig(jsonFile *string) *Config {
	var config Config
	var data []byte

	data, err = ioutil.ReadFile(*jsonFile)

	if err == nil {
		err = json.Unmarshal(data, &config)
	}

	if err != nil {
		ErrorLogger.Printf("config file could not be read")
		ErrorLogger.Fatal(err)
	}

	DebugLogger.Printf(
		"Loaded SMTP config for \"%s@%s:%d\"\n",
		config.Username,
		config.Host,
		config.Port,
	)
	return &config
}

func getTask(jsonFile *string) *Task {
	var task Task
	var data []byte

	data, err = ioutil.ReadFile(*jsonFile)

	if err == nil {
		err = json.Unmarshal(data, &task)
	}

	if err != nil {
		ErrorLogger.Printf("task file could not be read", *jsonFile)
		ErrorLogger.Fatal(err)
	}

	task.Recipientfile = adjustFilePath(jsonFile, &task.Recipientfile)
	task.Bodytemplate = adjustFilePath(jsonFile, &task.Bodytemplate)

	// adjust file paths
	attachments := task.Attachments
	for i := range attachments {
		attachments[i].Path = adjustFilePath(jsonFile, &attachments[i].Path)
	}

	DebugLogger.Printf("Loaded %s task \"%s\" from \"%s\"\n", Name, task.Name, *jsonFile)

	return &task
}

func getRecipients(jsonFile *string) []Recipient {
	var recipients []Recipient
	var data []byte

	data, err = ioutil.ReadFile(*jsonFile)

	if err == nil {
		err = json.Unmarshal(data, &recipients)
	}

	if err != nil {
		ErrorLogger.Printf("recipient file could not be read")
		ErrorLogger.Fatal(err)
	}

	// adjust file paths
	for i := range recipients {
		attachments := recipients[i].Attachments
		for j := range attachments {
			attachments[j].Path = adjustFilePath(jsonFile, &attachments[j].Path)
		}
	}

	DebugLogger.Printf("Loaded %d recipients from \"%s\"\n", len(recipients), *jsonFile)

	return recipients
}

func adjustFilePath(referenceFile *string, targetFile *string) string {
	return filepath.Join(filepath.Dir(*referenceFile), *targetFile)
}

func buildMessage(mail Mail) []byte {
	var buf bytes.Buffer

	// From:
	buf.WriteString(fmt.Sprintf("From: %s\r\n", mail.Sender))

	if mail.ReplyTo != "" {
		buf.WriteString(fmt.Sprintf("Reply-To: %s\r\n", mail.ReplyTo))
	}

	// Date:
	t := time.Now()
	buf.WriteString(fmt.Sprintf("Date: " + t.Format(time.RFC1123Z) + "\r\n"))

	// To:
	if len(mail.To) > 0 {
		buf.WriteString(fmt.Sprintf("To: %s\r\n", strings.Join(mail.To, ";")))
	}

	// Cc:
	if len(mail.Cc) > 0 {
		buf.WriteString(fmt.Sprintf("Cc: %s\r\n", strings.Join(mail.Cc, ";")))
	}

	// Subject:
	buf.WriteString(fmt.Sprintf("Subject: %s\r\n", mail.Subject))

	// Multipart header
	mw := multipart.NewWriter(&buf)
	contentType := "multipart/mixed"
	charset := "utf-8"
	boundary := mw.Boundary()

	if mail.Charset != "" {
		charset = mail.Charset
	}

	buf.WriteString("MIME-Version: 1.0\r\n")
	buf.WriteString(fmt.Sprintf("User-Agent: %s\r\n", UserAgent))
	buf.WriteString(
		fmt.Sprintf(
			"Content-Type: %s; charset=\"%s\"; boundary=\"%s\"\r\n",
			contentType,
			charset,
			boundary,
		),
	)
	buf.WriteString(fmt.Sprintf("Content-Disposition: %s\r\n", "inline"))
	buf.WriteString("\r\n")

	// Part of inline body
	pw, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":        {fmt.Sprintf("text/plain; charset=\"%s\"", charset)},
		"Content-Disposition": {"inline"},
	})

	if err != nil {
		ErrorLogger.Fatal(err)
	}

	fmt.Fprint(pw, mail.Text)

	// Remaining parts
	for i := range mail.Attachments {
		attachment := mail.Attachments[i]

		pw, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type": {
				fmt.Sprintf("%s; charset=\"%s\"", attachment.Type, attachment.Charset),
			},
			"Content-Transfer-Encoding": {"base64"},
			"Content-Disposition": {
				fmt.Sprintf("attachment; filename=\"%s\"", filepath.Base(attachment.Path)),
			},
		})

		if err != nil {
			ErrorLogger.Fatal(err)
		}

		data, err := ioutil.ReadFile(attachment.Path)

		if err != nil {
			ErrorLogger.Fatal(err)
		}

		pw.Write([]byte(fmt.Sprint(base64.StdEncoding.EncodeToString(data))))
	}

	mw.Close()

	return buf.Bytes()
}

func sendMail(config *Config, mail Mail, dryrun bool) {
	msg := buildMessage(mail)

	// Use [To...,Cc...,Bcc...] as RCPT TO, difference is resembled in mail header
	var rcptTo string = strings.Join(append(append(append([]string{}, mail.To...), mail.Cc...), mail.Bcc...), ",")

	if dryrun {
		fmt.Println(string(msg))
		DebugLogger.Printf("dryrun: not sending mail to %s", rcptTo)
		return
	}

	var client *smtp.Client

	servername := fmt.Sprintf("%s:%d", config.Host, config.Port)
	auth := smtp.PlainAuth("", config.Username, config.Password, config.Host)

	tlsconfig := &tls.Config{
		InsecureSkipVerify: false,
		ServerName:         config.Host,
	}

	// Figure out whether SSL or STARTTLS should be used
	if config.Type == "ssl" || config.Type == "tls" || config.Port == 465 {
		conn, err := tls.Dial("tcp", servername, tlsconfig)
		if err != nil {
			ErrorLogger.Fatal(err)
		}

		client, err = smtp.NewClient(conn, config.Host)
		if err != nil {
			ErrorLogger.Fatal(err)
		}

	} else if config.Type == "starttls" {
		client, err = smtp.Dial(servername)
		if err != nil {
			ErrorLogger.Fatal(err)
		}
		client.StartTLS(tlsconfig)
	}

	if client == nil || err != nil {
		ErrorLogger.Fatal(err)
	}

	// AUTH
	if err = client.Auth(auth); err != nil {
		ErrorLogger.Fatal(err)
	}

	// MAIL FROM
	if err = client.Mail(mail.Sender); err != nil {
		ErrorLogger.Fatal(err)
	}

	// RCPT TO
	if err = client.Rcpt(rcptTo); err != nil {
		ErrorLogger.Fatal(err)
	}

	// DATA
	w, err := client.Data()
	if err != nil {
		ErrorLogger.Fatal(err)
	}

	_, err = w.Write(msg)
	if err != nil {
		ErrorLogger.Fatal(err)
	}

	client.Quit()

	DebugLogger.Printf("Sent mail to %s", rcptTo)
}

func getBody(fileName *string) []byte {
	var template []byte

	template, err = ioutil.ReadFile(*fileName)

	if err != nil {
		ErrorLogger.Println("body template could not be read")
		ErrorLogger.Fatal(err)
	}

	DebugLogger.Printf("Loaded body template from \"%s\"\n", *fileName)

	return template

}

func processTemplate(template []byte, recipient Recipient) string {
	var replacements []string

	variables := recipient.Variables

	for i := range variables {
		variable := variables[i]
		replacements = append(replacements, variable.Name)
		replacements = append(replacements, variable.Value)
	}

	replacer := strings.NewReplacer(replacements...)

	return replacer.Replace(string(template))
}

func main() {
	DebugLogger.Println("Starting the application...")

	flag.Parse()

	if smtpConfigFile == "" || taskFile == "" {
		ErrorLogger.Print("Mandatory argument not provided. Exiting.\n\n")
		flag.Usage()
		os.Exit(1)
	}

	config := getSmtpConfig(&smtpConfigFile)

	task := getTask(&taskFile)
	recipients := getRecipients(&task.Recipientfile)
	template := getBody(&task.Bodytemplate)

	var mails []Mail

	for i := range recipients {
		recipient := recipients[i]

		mail := Mail{
			Sender:      task.Sender,
			To:          []string{recipient.Email},
			Cc:          task.Cc,
			Bcc:         []string{},
			ReplyTo:     task.ReplyTo,
			Subject:     task.Subject,
			Text:        processTemplate(template, recipient),
			Attachments: recipient.Attachments,
		}

		// task can specify attachments for all recipients
		if len(task.Attachments) > 0 {
			mail.Attachments = append(mail.Attachments, task.Attachments...)
		}

		mails = append(mails, mail)

	}

	// safety countdown
	if !dryRun {
		totalMails := len(recipients) + len(task.Cc) + len(task.Bcc)
		fmt.Printf("\nWARNING: You are going to automatically send %d mail(s):\n\n", totalMails)
		fmt.Printf("         Subject: \"%s\"\n\n", task.Subject)
		fmt.Printf("         From: \"%s\"\n", task.Sender)
		fmt.Printf("         Reply-To: \"%s\"\n", task.ReplyTo)
		fmt.Printf("         Cc: \"%s\"\n", task.Cc)
		fmt.Printf("         Bcc: \"%s\"\n", task.Bcc)
		fmt.Printf("         Global attachments: %d\n", len(task.Attachments))
		fmt.Printf("         To: \"%s\"\n", task.Recipientfile)
		fmt.Printf("         Text: \"%s\"\n\n", task.Bodytemplate)
		fmt.Printf("If you made ANY mistake, %d people will be angry at you.\n\n", totalMails)
		fmt.Printf("This is your last chance to cancel. Press Ctrl-C to cancel.\n\n")

		for i := countdown; i >= 0; i-- {
			fmt.Printf("\033[2K\rSafety Countdown: %d", i)
			time.Sleep(1 * time.Second)
		}

		fmt.Print("\n\n")
	}

	fmt.Println("Fire!")
	for i := range mails {

		// recovery phase to prevent triggering spam detection of smtp server
		if !dryRun && (i+1)%cooldown == 0 {
			fmt.Printf(
				"\nAutomatic cooldown for %d minutes to let smtp server recover.\n\n",
				cooldown,
			)
			for i := cooldown; i >= 0; i-- {
				fmt.Printf("\033[2K\rRemaining in recovery phase for %d minutes", i)
				time.Sleep(1 * time.Minute)
			}

			fmt.Print("\n\n")
			fmt.Println("Fire!")
		}

		sendMail(config, mails[i], dryRun)
		if dryRun {
			fmt.Printf("> %d of %d mails NOT sent (dry-run)\n", i+1, len(mails))
		} else {
			fmt.Printf("> %d of %d mails sent\n", i+1, len(mails))
		}
	}

}