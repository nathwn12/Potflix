# Third-party notices

## PotPlayer-DiscordRPC

Upstream: `ayunami2000/PotPlayer-DiscordRPC`
License: BSD 3-Clause
Copyright line (verbatim): `Copyright (c) 2023, ayunami2000`

The media-title cleaner logic in this repository derives from that project.

Full license text:

```
BSD 3-Clause License

Copyright (c) 2023, ayunami2000
All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.

2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

3. Neither the name of the copyright holder nor the names of its
   contributors may be used to endorse or promote products derived from
   this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```

## Metadata sources

The Rust app's Rich Presence card resolves titles through these keyless
third-party sources (no credentials, no scraping — public APIs only). The
frozen Go reference used TVmaze, AniList, Jikan, iTunes and Wikipedia; the
Rust app uses TVmaze, AniList, Tenrai, Kitsu, Cinemeta, Wikipedia, JustWatch,
Episodate, Bangumi, LiveChart.me and ANN.

- **TVmaze** (`https://api.tvmaze.com`) — TV series names, episode tables,
  posters and summaries. TVmaze's API docs license the data
  *"licensed by CC BY-SA"*; attribute TVmaze wherever the card data is
  republished.
- **AniList** (`https://graphql.anilist.co`) — anime names, episode lists and
  artwork. AniList's Terms (fetched 2026-10-15) allow free non-commercial use
  (commercial use under $150/month revenue needs no express permission) and
  state: using the API *"as a backup or data storage service is strictly
  prohibited. Hoarding or mass collection of data from the AniList API is
  strictly prohibited."* Read with its neighbours — *backup … service*,
  *hoarding*, *mass collection* — that clause targets re-hosting AniList's
  database and bulk scraping, not a client's offline copy of what the user is
  being shown; no fetched page mentions caching. The app stays on that side of
  the line: a bounded 30-day read-through cache
  (`%LOCALAPPDATA%\Potflix\meta`), keyed to the user's own titles so the card
  works offline and the provider is not re-queried for the same title. Nothing
  cached is republished, redistributed, or bulk-collected. Fetch spacing is
  2000 ms (30/min), matching the documented current degraded limit.
- **Cinemeta** (`https://v3-cinemeta.strem.io`) — Stremio's default catalog
  addon (`com.linvo.cinemeta`): movie and series lookup by IMDb id, with
  TMDB-derived descriptions, ratings, genres and runtimes. Artwork is served
  from `m.media-amazon.com` and `images.metahub.space`. Replaces the iTunes
  movie leg (which returned the first `feature-movie` row with no title
  check) and the Jikan anime leg (discontinued 2026-10-01;
  `api.jikan.moe` no longer answers).
- **Wikipedia** (`https://en.wikipedia.org`, Action API) — movie fallback
  (summary, poster). Article text and images are CC BY-SA; attribute
  Wikipedia when republishing card data.
- **JustWatch** (`https://apis.justwatch.com`, unofficial GraphQL) — movie
  title search (third movie leg). Unofficial and undocumented; may change
  without notice.
- **Episodate** (`https://www.episodate.com`) — TV series names and episode
  tables. A small community database; unofficial, may change without notice.
- **Bangumi** (`https://api.bgm.tv`, keyless subject search) — anime and
  live-TV subjects (cover, titles, episode count, air date, synopsis in one
  call). Display title is the native `name`, falling back to the Chinese
  title.
- **LiveChart.me** (`https://www.livechart.me`) — anime names and episode
  tables. Unofficial; may change without notice.
- **ANN** (`https://cdn.animenewsnetwork.com`, encyclopedia API) — anime
  names, episode counts and summaries. Unofficial; may change without
  notice.

## Bundled logo fonts

The font files in `design\fonts\` are used solely to render the Potflix
logo/artwork offline via `design\make_logo.py`. Each is licensed under the
SIL Open Font License 1.1. The full license text lives in the per-family
files in `design\fonts\` and is not repeated here. The fonts are not
redistributed as the app's own typeface.

- `design\fonts\ArchivoBlack-Regular.ttf`
  License: `design\fonts\archivoblack-OFL.txt`
  Copyright line (verbatim): `Copyright 2017 The Archivo Black Project Authors (https://github.com/Omnibus-Type/ArchivoBlack)`
- `design\fonts\Montserrat-Variable.ttf`
  License: `design\fonts\montserrat-OFL.txt`
  Copyright line (verbatim): `Copyright 2024 The Montserrat.Git Project Authors (https://github.com/JulietaUla/Montserrat.git)`
- `design\fonts\Poppins-ExtraBold.ttf`, `design\fonts\Poppins-Black.ttf`
  License: `design\fonts\poppins-OFL.txt`
  Copyright line (verbatim): `Copyright 2020 The Poppins Project Authors (https://github.com/itfoundry/Poppins)`
- `design\fonts\Anton-Regular.ttf`
  License: `design\fonts\anton-OFL.txt`
  Copyright line (verbatim): `Copyright 2020 The Anton Project Authors (https://github.com/googlefonts/AntonFont.git)`
- `design\fonts\BebasNeue-Regular.ttf`
  License: `design\fonts\bebasneue-OFL.txt`
  Copyright line (verbatim): `Copyright © 2010 by Dharma Type.`
- `design\fonts\Inter-Variable.ttf`
  License: `design\fonts\inter-OFL.txt`
  Copyright line (verbatim): `Copyright 2020 The Inter Project Authors (https://github.com/rsms/inter)`

Reserved Font Names: none of the six `<family>-OFL.txt` files declares a
Reserved Font Name after its copyright statement; the only "Reserved Font
Name" wording in each file is the generic OFL definition
(`"Reserved Font Name" refers to any names specified as such after the
copyright statement(s).`). Per the OFL, the names of the fonts'
Copyright Holders and Authors must not be used to promote, endorse, or
advertise a Modified Version, so the rendered Potflix wordmark must not
imply the fonts' authors endorse Potflix.
