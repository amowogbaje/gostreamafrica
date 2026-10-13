package auth

import "strings"

// commonPasswords is a small blocklist of very common passwords that are at least 10
// characters long (shorter ones are already rejected by the length rule). It is a
// speed bump, not a substitute for rate limiting; swap in a larger list or a
// breached-password check later.
var commonPasswords = func() map[string]struct{} {
	list := []string{
		"password123", "password1234", "password12345", "password123!", "password@123", "password!23",
		"passw0rd123", "p@ssw0rd123", "p@ssword123", "mypassword", "mypassword1", "mypassword123",
		"passwordpassword", "pass1234567", "changeme123", "changemenow", "letmein1234", "letmein123!",
		"welcome123", "welcome1234", "welcome12345", "admin12345", "administrator", "administrator1",
		"1234567890", "12345678910", "0123456789", "1234567891", "9876543210", "0987654321",
		"1111111111", "0000000000", "1234512345", "123123123123", "123456789a", "a123456789",
		"qwertyuiop", "qwerty12345", "qwerty123456", "qwertyuiop1", "qwertyuiop123", "1q2w3e4r5t",
		"q1w2e3r4t5", "1qaz2wsx3edc", "qazwsxedcrfv", "zaq12wsxcde3", "asdfghjkl1", "asdfghjkl123",
		"zxcvbnm123", "abcdefghij", "abcd123456", "abc1234567", "abcdefg123",
		"iloveyou12", "iloveyou123", "iloveyou1234", "loveyou1234", "ilovegod123", "godislove123",
		"godisgood123", "jesus12345", "blessed1234", "football123", "footballer1", "baseball123",
		"superman123", "princess123", "sunshine123", "trustno1234", "monkey12345", "dragon12345",
		"master12345", "shadow12345", "michael1234", "jennifer123", "computer123", "internet123",
		"whatever123", "nigeria123", "nigerian123", "naija12345", "lagos12345", "abuja12345",
		"ghana12345", "streamafrica", "streamafrica1", "streamafrica123", "streamafrica2024",
		"streamafrica2025", "streamafrica2026", "nollywood123", "afrobeats123",
	}
	m := make(map[string]struct{}, len(list))
	for _, p := range list {
		m[p] = struct{}{}
	}
	return m
}()

func isCommonPassword(pw string) bool {
	_, ok := commonPasswords[strings.ToLower(pw)]
	return ok
}
