# webfit

`webfit` is a small password-protected web app for resizing and cropping images.

## Setup

Create the environment file and data directory:

```sh
go run . init
```

This creates:

- `./config/.env`
- `./data/main.sqlite` when the app starts

Edit `./config/.env` and change `ADMIN_PASSWORD` before exposing the app.

## Run

```sh
go run .
```

Optional address:

```sh
go run . -addr 0.0.0.0:8787
```

The app requires login and accepts JPEG, PNG, or WebP uploads. The resize tool
supports standard web targets and custom widths. The crop tool includes an
interactive crop box, common aspect-ratio presets, exact pixel controls, and
JPEG, PNG, or WebP export. Uploaded images are not stored.

Standard targets:

- Hero: `1920px` wide
- Wide content: `1440px` wide
- Card: `1200px` wide
- Article: `960px` wide
- Social post: `1080px` wide
- Thumbnail: `480px` wide
- Icon: `256px` wide

The browser reads the selected image dimensions and suggests a reasonable target before upload.

## Environment

```env
ADMIN_USERNAME=admin
ADMIN_PASSWORD=change-me-now
SESSION_SECRET=<random-secret>
```

The app reads `./config/.env` by default and stores data at `./data/main.sqlite`.
# webft
