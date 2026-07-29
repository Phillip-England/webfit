# webfit

`webfit` is a small password-protected image resizing web app.

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
go run . -env ./config/.env
```

Optional address:

```sh
go run . -env ./config/.env -addr 0.0.0.0:8787
```

The app requires login, accepts a JPEG, PNG, or WebP upload, resizes it to a standard web target or custom width, and downloads the resized result. Uploaded images are not stored. WebP uploads are exported as JPEG.

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
DB_PATH=../data/main.sqlite
```

`DB_PATH` may be relative to the environment file location.
# webft
