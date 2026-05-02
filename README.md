# Telegram Bot Gemini CLI

Bot Telegram sederhana yang menjalankan perintah `gemini-cli` berdasarkan input teks dari user dan mengembalikan hasilnya.

## Persyaratan
- [Go](https://golang.org/doc/install) (versi 1.16 atau lebih baru)
- [gemini-cli](https://github.com/google/gemini-cli) sudah terinstall dan dapat dijalankan di terminal.
- Token Bot Telegram (didapat dari [@BotFather](https://t.me/BotFather)).

## Instalasi

1. Clone repository ini atau copy kodenya.
2. Jalankan perintah berikut untuk mengunduh dependency:
   ```bash
   go mod tidy
   ```

## Cara Menjalankan

### Linux / macOS
```bash
export TELEGRAM_BOT_TOKEN="TOKEN_ANDA_DISINI"
go run main.go
```

### Windows (PowerShell)
```powershell
$env:TELEGRAM_BOT_TOKEN="TOKEN_ANDA_DISINI"
go run main.go
```

## Cara Penggunaan
Kirim pesan teks apa saja ke bot Anda di Telegram. Bot akan mengeksekusi:
`gemini-cli "[pesan anda]"`
dan mengirimkan outputnya kembali kepada Anda.
