package model

import "testing"

func TestValidateRegistration(t *testing.T) {
	tests := []struct {
		name     string
		email    string
		password string
		userName string
		wantErr  bool
	}{
		{name: "合法的註冊資料", email: "a@example.com", password: "12345678", userName: "Amy"},
		{name: "email 格式錯誤", email: "not-an-email", password: "12345678", userName: "Amy", wantErr: true},
		{name: "email 帶顯示名稱不接受", email: "Amy <a@example.com>", password: "12345678", userName: "Amy", wantErr: true},
		{name: "密碼太短", email: "a@example.com", password: "1234567", userName: "Amy", wantErr: true},
		{name: "密碼超過 bcrypt 上限", email: "a@example.com", password: string(make([]byte, 73)), userName: "Amy", wantErr: true},
		{name: "沒有姓名", email: "a@example.com", password: "12345678", userName: "  ", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRegistration(tt.email, tt.password, tt.userName)
			if (err != nil) != tt.wantErr {
				t.Fatalf("wantErr=%v, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestNormalizeEmail(t *testing.T) {
	if got := NormalizeEmail("  Amy@Example.COM "); got != "amy@example.com" {
		t.Fatalf("got %q", got)
	}
}
