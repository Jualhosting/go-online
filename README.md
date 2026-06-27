# Product Knowledge & Architecture Blueprint: goinstant.my.id

## 1. Vision & Core Value
`goinstant.my.id` adalah sebuah platform SaaS infrastruktur (Developer Tools) modern berbasis Go (Golang) yang merangkap dua fungsi utama:
- **Instant Tunneling (Ngrok-style)**: Mengekspos aplikasi localhost pengembang ke internet publik menggunakan protokol berkecepatan tinggi secara zero-config.
- **Instant Static Deployment (Netlify-style)**: Mengunggah dan meng-host file statis (HTML/CSS/JS) ke cloud global dalam hitungan detik.

**Prinsip Utama**: Klien/Pengguna akhir TIDAK BOLEH direpotkan untuk menginstal runtime language apa pun (seperti Go, Node.js, Python) di laptop mereka. Mereka cukup mengunduh satu file biner hasil kompilasi tunggal (Pre-compiled Single Binary) dari Go yang langsung bisa dieksekusi.

---

## 2. System Architecture & Network Flow
Infrastruktur jaringan berjalan di atas AWS EC2 (VPS) dan terintegrasi dengan Cloudflare R2. Port yang dibuka pada firewall AWS Security Group adalah **FINAL/SELESAI** dan tidak boleh ditambah:
- **Port 80 (TCP)**: HTTP Publik / ACME HTTP Challenge.
- **Port 443 (TCP)**: HTTPS Publik / Jalur Webhook, Web Traffic, dan Static Files.
- **Port 9000 (UDP)**: Core Tunneling Connection. Jalur pipa khusus berbasis protokol QUIC/UDP tempat CLI lokal terhubung ke VPS server.

### A. Alur Kerja Fitur `expose` (Tunneling)
1. Pengguna menjalankan perintah di terminal lokal:
   ```bash
   goinstant expose --port 8080 --subdomain toko-syafri
   ```
2. CLI lokal membuka koneksi persistent stream via **UDP Port 9000 (QUIC)** ke MTM-Server di VPS AWS.
3. VPS menangkap domain `toko-syafri.goinstant.my.id`. Melalui pustaka **CertMagic (Go)**, server langsung mengurus SSL otomatis secara real-time ke Let's Encrypt lewat **port 443**.
4. Publik mengakses `https://toko-syafri.goinstant.my.id` $\rightarrow$ VPS menerima trafik di **Port 443** $\rightarrow$ Diteruskan via pipa **UDP 9000** $\rightarrow$ Sampai di `localhost:8080` pengguna.

### B. Alur Kerja Fitur `deploy` (Web Static)
1. Pengguna menjalankan perintah di terminal lokal:
   ```bash
   goinstant deploy --dir "./dist" --subdomain portofolio
   ```
2. CLI lokal mengompresi dan mengunggah file statis via **HTTP POST** ke VPS (**Port 443 TCP**).
3. MTM-Server di VPS menerima file, lalu langsung melempar dan menyimpannya ke **Cloudflare R2 Object Storage** melalui API resmi.
4. Situs online selamanya di `https://portofolio.goinstant.my.id`. Saat diakses, server membaca dari R2 (Zero Egress Fee / Gratis Bandwidth). Laptop pengguna bisa dimatikan dengan aman.

---

## 3. Technical Specifications & Tech Stack
- **Backend Engine (Server & CLI)**: Go (Golang) murni.
- **SSL Automation**: Pustaka Go `github.com/caddyserver/certmagic` untuk On-Demand TLS di level kode server (Tanpa perlu biner Caddy terpisah, tanpa Certbot).
- **Network Protocol**: QUIC / WebSockets (Go-native) untuk menembus NAT/Firewall ISP rumahan klien.
- **Server Deployment**: Docker Container (`go-online-mtm-server`) berjalan di AWS EC2 Ubuntu.
- **Storage Provider**: Cloudflare R2 untuk efisiensi biaya penyimpanan static web asset.
- **Database Konfigurasi**: SQLite / Cloudflare D1 untuk menyimpan mapping subdomain pengguna secara instan.

---

## 4. Client Delivery & Packaging Policy
- **Cross-Compilation**: Dockerfile di VPS wajib melakukan cross-compile otomatis saat proses build server untuk menghasilkan biner 3 OS utama: `goinstant-windows.exe`, `goinstant-linux`, dan `goinstant-darwin`.
- **Distribution Mode**:
  - **Portable Mode**: Pengguna cukup mengunduh biner dan menjalankannya langsung di dalam folder proyek menggunakan perintah `.\goinstant.exe`.
  - **Global CLI Mode (Installer Script)**: Disediakan file `install.ps1` (Windows) dan `install.sh` (Linux/Mac) di rute `/downloads/` untuk otomatis mengunduh biner dan mendaftarkannya ke sistem PATH lingkungan pengguna. Setelah itu, pengguna bisa mengetik perintah bersih: `goinstant expose` atau `goinstant deploy` secara global tanpa embel-embel eksekusi file lokal.

---

## 5. Instruction for the AI Code Assistant
- **JANGAN** menyarankan penambahan port terbuka baru di AWS selain 80, 443, dan 9000 (UDP).
- **JANGAN** membuat konfigurasi yang mengharuskan klien lokal menginstal dependensi compiler pemrograman eksternal.
- Gunakan efisiensi konkurensi Go (*goroutines*) untuk menangani multiplexing koneksi hulu-hilir (Port 443 $\leftrightarrow$ Port 9000).
- Seluruh domain, routing, dan traffic handling wajib mengacu pada domain utama saat ini: `goinstant.my.id` dan wildcard-nya `*.goinstant.my.id`.
- **Ikuti arsitektur dan product knowledge ini dengan disiplin. Jangan keluar dari jalur ini.**
