# S5 version admission

Status: candidate for root proof. Rows below were resolved from retained primary-source dossiers observed on 2026-10-15
(listed under Sources). Nothing here is a runtime pass: `scripts/s5-toolchain.sh --admission` re-checks every row against
the Dockerfile, `web/package.json`, `web/bun.lock`, `go.mod` and the installed tools, applies the maintenance rule of the
row's category (ADR 0011) and only that run may print `S5-ADMISSION-PASS`. The root observed the eight web tools run on Bun 1.4.3
on 2026-10-16 (EVID-002, SLICE-002): all eight passed, so the npm rows carry `executed EVID-002 2026-10-16`. A row may still
carry `pending-SLICE-002` before a result is recorded; both forms pass the admission check, an executed date must be a real
calendar date, and the source metadata alone never claims Bun compatibility.

The tables are machine-read. One row per introduced direct package/tool/image; columns:
`ID | kind | category | name | version | pin | source | publisher | licence | maintenance | targets | bun`.

- Categories and their only allowed kinds (ADR 0011, owner question q-2026-10-15-001): `base-image` (image),
  `archive-snapshot` (snapshot, provenance identity only), `direct-tool` (archive, apt or apt-fetch: go, bun, golangci-lint,
  chrome-headless-shell, gcc, libc6-dev, qemu-user, unzip), `go-dependency` (go), `npm-dependency` (npm) and the Debian OS
  categories `os-library` (apt, installed `Section: libs`), `os-font` (apt, `Section: fonts`) and `os-data` (apt,
  `ca-certificates` or `tzdata` only). Anything ambiguous takes the stricter non-OS category.
- `maintenance` for `base-image`, `direct-tool`, `go-dependency` and `npm-dependency` is
  `release|upload|commit <immutable id> <YYYY-MM-DD> <https URL> work=<subject>/<version>`: actual attributable work dated
  within 365 days before the run date. For apt rows `<subject>` is the Debian source package and `<version>` is the version of
  that work, which may be newer than the installed pin; the version, pin and source cells keep the exact installed pin and are
  never presented as released on the evidence date. For npm rows the immutable id is the registry `dist.integrity` of that
  exact release. A snapshot date, URL or identifier is never maintenance evidence.
- `maintenance` for OS rows is `distro-support debian <VERSION_ID> <codename> security-until=<date> <release URL>
  tracker=<security-tracker source-package URL> lists=<support-list commit> status=<clear|supported|ends-DATE>`. It records
  supported-Debian/security-maintenance coverage only, never absence of vulnerabilities.
- Debian rows: `source` is `debian-source <source package> <source version> <snapshot URL>`, equal to the installed
  `${source:Package}`/`${source:Version}`; `licence` names the licences read from the package's copyright file followed by
  `copyright-sha256=<hex>` and `dep5=<set>` (the sorted DEP-5 `License:` values, `_` for spaces, or `none`), both recomputed
  from the installed file. Both tokens are installed-image measurements already recorded in the rows (see Installed measurements).

## Inventory

| ID | kind | category | name | version | pin | source | publisher | licence | maintenance | targets | bun |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| ADM-001 | image | base-image | debian | trixie-slim | sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f | https://hub.docker.com/_/debian (index digest; amd64 manifest sha256:918311b7b6c4c6f68b232ba516584925f6c78ad82b6fd534b98979df6438e483) | Debian project via Docker Official Images (debuerreotype/docker-debian-artifacts) | no single licence: Debian Social Contract guidelines (https://www.debian.org/social_contract#guidelines); per-package copyright files govern the contents | commit 6b287dc80f463f088b8d6432ceeb633ec0c0b8a4 2026-10-05 https://github.com/docker-library/official-images/commit/6b287dc80f463f088b8d6432ceeb633ec0c0b8a4 work=debian/trixie-20261005-slim | linux/amd64 | n/a |
| ADM-002 | snapshot | archive-snapshot | debian-snapshot | 20261018T000000Z | 20261018T000000Z | http://snapshot.debian.org/archive/debian/20261018T000000Z/ and debian-security | Debian snapshot.debian.org archive | Debian main (provenance identity only; per-package copyright files govern contents) | identity 20261018T000000Z http://snapshot.debian.org/archive/debian/20261018T000000Z/ | linux/amd64 | n/a |
| ADM-003 | archive | direct-tool | go | 1.27.2 | ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fe5 | https://go.dev/dl/go1.27.2.linux-amd64.tar.gz | The Go Authors (Google) | BSD-3-Clause | release 022c8636110ebac86a9b88724cda168ed713c21f 2026-10-08 https://go.dev/doc/devel/release#go1.27.2 work=go/1.27.2 | linux/amd64 host; cross-compiles all eight committed targets | n/a |
| ADM-004 | archive | direct-tool | bun | 1.4.3 | 3e730528d89775f03f87f05c06787c4df4099d0a37f3f252e7b829cb23816e18 | https://github.com/oven-sh/bun/releases/download/bun-v1.4.3/bun-linux-x64.zip | Oven (oven-sh) | MIT (statically links LGPL-2 JavaScriptCore and other bundled libraries; no single SPDX id) | release c6da4a4d3010e5553438c60f6bd76d981976867c 2026-10-10 https://github.com/oven-sh/bun/releases/tag/bun-v1.4.3 work=bun/1.4.3 | linux/amd64 | admission-executes-bun; web tools executed EVID-002 2026-10-16 |
| ADM-005 | archive | direct-tool | golangci-lint | 2.14.0 | ab90aeb7b066f92a33415b638a50fe5344bbb75a0d32ad30cc248d88f81032ab | https://github.com/golangci/golangci-lint/releases/download/v2.14.0/golangci-lint-2.14.0-linux-amd64.tar.gz | golangci (golangci-lint project) | GPL-3.0-only (development tool, never distributed) | release 114493f9b3e7257d29e4130f2b4a4aadefbb6845 2026-09-24 https://github.com/golangci/golangci-lint/releases/tag/v2.14.0 work=golangci-lint/2.14.0 | linux/amd64 | n/a |
| ADM-006 | archive | direct-tool | chrome-headless-shell | 156.0.8078.4 | c3e50596aef52c58f8fda3c6b3411c33d34c88b177b3ed1c2243359fd3b8ba20 | https://storage.googleapis.com/chrome-for-testing-public/156.0.8078.4/linux64/chrome-headless-shell-linux64.zip | Google (Chrome for Testing; Playwright's chromium-headless-shell revision 1248 resolves this archive) | BSD-3-Clause plus bundled third-party notices (LICENSE.headless_shell in archive) | upload 1790731396923999 2026-09-30 https://storage.googleapis.com/chrome-for-testing-public/156.0.8078.4/linux64/chrome-headless-shell-linux64.zip work=chrome-headless-shell/156.0.8078.4 | linux/amd64 | n/a |
| ADM-010 | apt | os-data | ca-certificates | 20250419 | snapshot:20261018T000000Z | debian-source ca-certificates 20250419 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | GPL-2+ (debian/*, examples/*); MPL-2.0 (mozilla/certdata.txt, mozilla/nssckbi.h) copyright-sha256=e85e1bcad3a915dc7e6f41412bc5bdeba275cadd817896ea0451f2140a93967c dep5=GPL-2+,MPL-2.0 | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/ca-certificates lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-011 | apt | os-font | fonts-liberation | 1:2.1.5-3 | snapshot:20261018T000000Z | debian-source fonts-liberation 1:2.1.5-3 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | SIL-OFL-1.1 (Files: *); GPL-2+ (debian/*) copyright-sha256=f32df582a2aab128c07b2f7e36e7b928dc5f6bd82657b905c04f91459f9e5cf9 dep5=GPL-2+,SIL-OFL-1.1 | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/fonts-liberation lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-012 | apt | direct-tool | gcc | 4:14.2.0-1 | snapshot:20261018T000000Z | debian-source gcc-defaults 1.220 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian (compiler metapackage; the exact installed pin is gcc-defaults 1.220 and is not claimed released on the evidence date) | GPL-2+ (gcc-defaults packaging; free-text copyright); c89/c99 man pages carry a NetBSD 4-clause BSD-style notice; the compiler code is source gcc-14 (GPL-3+ with the GCC Runtime Library Exception 3.1) copyright-sha256=156004744edbf17a975f0004ac4a9295b160ab403d646c445235711255ca9ff8 dep5=none | upload 129064bfee8b0b638f68d8ce8957264501f04483 2026-08-02 https://metadata.ftp-master.debian.org/changelogs/main/g/gcc-defaults/gcc-defaults_1.233_changelog work=gcc-defaults/1.233 | linux/amd64 | n/a |
| ADM-013 | apt | os-library | libasound2t64 | 1.2.14-1+deb13u1 | snapshot:20261018T000000Z | debian-source alsa-lib 1.2.14-1+deb13u1 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | LGPL-2.1+ copyright-sha256=28615d5f059d212259b6a82f6326daf8753349df8dfbdcd108310460ed2ec04f dep5=LGPL-2.1+ | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/alsa-lib lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-014 | apt | os-library | libatk-bridge2.0-0t64 | 2.56.2-1+deb13u2 | snapshot:20261018T000000Z | debian-source at-spi2-core 2.56.2-1+deb13u2 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | LGPL-2+, LGPL-2.1+, AFL-2.1 or GPL-2+, GPL-2 (debian/*); also GPL-2+, public-domain, AFL-2.1 copyright-sha256=72fbc9d2c44016dd061f3f63f75253e57089bcd23a4f9023039de4631e7cf918 dep5=AFL-2.1,AFL-2.1_or_GPL-2+,GPL-2,GPL-2+,LGPL-2+,LGPL-2.1+,public-domain | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/at-spi2-core lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-015 | apt | os-library | libatk1.0-0t64 | 2.56.2-1+deb13u2 | snapshot:20261018T000000Z | debian-source at-spi2-core 2.56.2-1+deb13u2 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | LGPL-2+, LGPL-2.1+, AFL-2.1 or GPL-2+, GPL-2 (debian/*); also GPL-2+, public-domain, AFL-2.1 copyright-sha256=72fbc9d2c44016dd061f3f63f75253e57089bcd23a4f9023039de4631e7cf918 dep5=AFL-2.1,AFL-2.1_or_GPL-2+,GPL-2,GPL-2+,LGPL-2+,LGPL-2.1+,public-domain | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/at-spi2-core lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-016 | apt | os-library | libatspi2.0-0t64 | 2.56.2-1+deb13u2 | snapshot:20261018T000000Z | debian-source at-spi2-core 2.56.2-1+deb13u2 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | LGPL-2+, LGPL-2.1+, AFL-2.1 or GPL-2+, GPL-2 (debian/*); also GPL-2+, public-domain, AFL-2.1 copyright-sha256=72fbc9d2c44016dd061f3f63f75253e57089bcd23a4f9023039de4631e7cf918 dep5=AFL-2.1,AFL-2.1_or_GPL-2+,GPL-2,GPL-2+,LGPL-2+,LGPL-2.1+,public-domain | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/at-spi2-core lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-017 | apt | direct-tool | libc6-dev | 2.41-12+deb13u4 | snapshot:20261018T000000Z | debian-source glibc 2.41-12+deb13u4 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian (C library development files) | LGPL-2.1+ (Files: *); the copyright file lists 64 License: lines including GPL-2+, GPL-3+, link-exception variants, BSD-3-clause-Berkeley, ISC, BSL-1.0, Unicode-DFS-2016, CORE-MATH, SunPro, Carnegie and Inner-Net; not narrowed to the files libc6-dev ships copyright-sha256=f5788886720a2605a946e81d571e6c8162b09f58d2e2ceb8d36e5768fccd850d dep5=BSD-2-clause,BSD-2-clause_or_GPL-2,BSD-3-clause-Berkeley,BSD-3-clause-Berkeley_and_BSD-3-clause-WIDE,BSD-3-clause-Berkeley_and_DEC,BSD-3-clause-Berkeley_and_DEC_and_IBM_and_ISC,BSD-3-clause-Berkeley_and_DEC_and_ISC,BSD-3-clause-Berkeley_and_DEC_and_ISC_and_LGPL-2.1+,BSD-3-clause-Berkeley_and_DEC_and_LGPL-2.1+,BSD-3-clause-Berkeley_and_ISC,BSD-3-clause-Berkeley_and_LGPL-2.1+,BSD-3-clause-Berkeley_and_LGPL-2.1+_and_Univ-Coimbra,BSD-3-clause-Carnegie,BSD-3-clause-Oracle,BSD-3-clause-WIDE,BSD-like-Spencer,BSL-1.0,CORE-MATH,Carnegie,Carnegie_and_LGPL-2.1+,DEC,FSFAP,GPL-2,GPL-2+,GPL-2+-with-link-exception,GPL-3+,IBM,IBM_and_ISC,ISC,ISC_and_LGPL-2.1+,Inner-Net,Inner-Net_and_LGPL-2.1+,LGPL-2+,LGPL-2.1+,LGPL-2.1+-with-link-exception,LGPL-2.1+_and_GPL-2,LGPL-2.1+_and_SunPro,LGPL-3+,MIT-like-Lord,PCRE,SunPro,Unicode-DFS-2016,Univ-Coimbra,public-domain | upload f291d4082eef7e094241a1a87585f275cf336149 2026-06-29 https://metadata.ftp-master.debian.org/changelogs/main/g/glibc/glibc_2.41-12+deb13u4_changelog work=glibc/2.41-12+deb13u4 | linux/amd64 | n/a |
| ADM-018 | apt | os-library | libcairo2 | 1.18.4-1+b1 | snapshot:20261018T000000Z | debian-source cairo 1.18.4-1 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | LGPL-2.1 or MPL-1.1 per file (free-text copyright; some files carry other permissive notices) copyright-sha256=408fb9e704e875cd42d432e7c9b3b378f568e3c89db2b5c5ef3d11c025319574 dep5=none | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/cairo lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-019 | apt | os-library | libcups2t64 | 2.4.10-3+deb13u2 | snapshot:20261018T000000Z | debian-source cups 2.4.10-3+deb13u2 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | Apache-2.0-with-GPL2-LGPL2-Exception (Files: *); Zlib, Apache-2.0, FSFUL, BSD-2-Clause copyright-sha256=82dabde9d9e18c5929f320e274095627f2ed1c45153fd4744ab884adac65c425 dep5=Apache-2.0,Apache-2.0-with-GPL2-LGPL2-Exception,Apache-2.0-with-GPL2-LGPL2-Exception_or_BSD-2-clause,BSD-2-Clause,FSFUL,Zlib | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/cups lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-020 | apt | os-library | libdbus-1-3 | 1.16.2-2 | snapshot:20261018T000000Z | debian-source dbus 1.16.2-2 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | GPL-2+ or AFL-2.1 (library; Expat and Tcl-BSDish for other parts); LGPL-2.1+, BSD-3-clause, g10-permissive copyright-sha256=f5fd0bfcceb21f4c23a517368abcbfe8a4c1cfb3b521757b50fae4898840fe35 dep5=AFL-2.1,BSD-3-clause,BSD-3-clause-generic,BSD-3-clause_and_BSD-3-clause-generic,Expat,GPL-2+,GPL-2+_or_AFL-2.1,_and_Expat_and_Tcl-BSDish,LGPL-2.1+,Tcl-BSDish,g10-permissive | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/dbus lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-021 | apt | os-library | libdrm2 | 2.4.124-2 | snapshot:20261018T000000Z | debian-source libdrm 2.4.124-2 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | MIT/X11-style permission notices (free-text copyright; Debian names no licence) copyright-sha256=512c9a57fd8b449aa157351da9df07bf8e19f767e496a72205a4020268ddb2e0 dep5=none | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/libdrm lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-022 | apt | os-library | libexpat1 | 2.9.0-1~deb13u1 | snapshot:20261018T000000Z | debian-source expat 2.9.0-1~deb13u1 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | MIT copyright-sha256=60919fe1a156395ff14511bb6ff79756c50ade2fff59ac60c9bae1d8a7fe6292 dep5=MIT | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/expat lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-023 | apt | os-library | libfontconfig1 | 2.15.0-2.3 | snapshot:20261018T000000Z | debian-source fontconfig 2.15.0-2.3 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | Keith Packard permission notice (free-text copyright; Debian names no licence) copyright-sha256=b215a61cdd3e62b5b17cc28b1852c78acb3dd38be0fb30706f7efc050dba91db dep5=none | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/fontconfig lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-024 | apt | os-library | libfreetype6 | 2.13.3+dfsg-1+deb13u1 | snapshot:20261018T000000Z | debian-source freetype 2.13.3+dfsg-1+deb13u1 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | FTL (Files: *); Expat, Public-Domain, Zlib, BSD-3-Clause, FSFAP, GPL-2+, GPL-3+, MIT-SMC, BSL-1.0, MIT-Modern-Variant, OpenGroup-MIT copyright-sha256=acdab1506824347784d2cc0a634cfa17dbf525c0d5b4902b4ac9d0d4e1fed6c7 dep5=BSD-3-Clause,BSL-1.0,Expat,FSFAP,FTL,FTL_and_Expat,GPL-2+,GPL-3+,MIT-Modern-Variant,MIT-SMC,OpenGroup-MIT,Public-Domain,Zlib | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/freetype lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-025 | apt | os-library | libgbm1 | 25.0.7-2+deb13u1 | snapshot:20261018T000000Z | debian-source mesa 25.0.7-2+deb13u1 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | MIT (Files: *; no gbm-specific stanza); file also lists BSD-3-google, GPL-2, GPL-1+, Khronos, Apache-2.0, BSL, MLAA, SGI, BSD-2-clause, GPL copyright-sha256=55268f06fbc193ee1a4fe694045a9cec58ba727d74bb17b5b1d54de36873a038 dep5=Apache-2.0,BSD-2-clause,BSD-3-google,BSL,GPL,GPL-1+,GPL-2,GPL-2_or_MIT,Khronos,MIT,MLAA,SGI | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/mesa lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-026 | apt | os-library | libglib2.0-0t64 | 2.84.4-3~deb13u5 | snapshot:20261018T000000Z | debian-source glib2.0 2.84.4-3~deb13u5 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | LGPL-2.1+ (Files: *); AFL-2.0 or LGPL-2.1+, CC0-1.0, Unicode-DFS-2016, Expat, GPL-2+, LGPL-3+, FSFULLR, Iconv-PD, Janik-permissive, old-glib-tests copyright-sha256=b9fe78c8a0720c051c9ac8e8c6f25663b54f228e501d24fc0ba89c66cec7f7c1 dep5=AFL-2.0,AFL-2.0_or_LGPL-2.1+,Apache-2.0_with_LLVM_exception,CC-BY-SA-3.0,CC0-1.0,CC0-1.0_or_Mingw-PD,Expat,FSFULLR,GPL-2+,Iconv-PD,Janik-permissive,Janik-permissive_and_old-glib-tests,Kuchling-PD,LGPL-2+,LGPL-2.1+,LGPL-2.1+_and_Kuchling-PD_and_Plumb-PD,LGPL-3+,Mingw-PD,Plumb-PD,Unicode-DFS-2016,bzip2-1.0.6,cmph,old-glib-tests | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/glib2.0 lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-027 | apt | os-library | libnspr4 | 2:4.36-1 | snapshot:20261018T000000Z | debian-source nspr 2:4.36-1 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | MPL-2.0 copyright-sha256=3c4fcc131809b2b6993513bfe4cf6f1ede391ec84f1d5ceca78fb75537b22452 dep5=MPL-2.0 | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/nspr lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-028 | apt | os-library | libnss3 | 2:3.110-1+deb13u4 | snapshot:20261018T000000Z | debian-source nss 2:3.110-1+deb13u4 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | MPL-2.0 (Files: *); Zlib, BSD-3, public-domain copyright-sha256=3f450f9453233852b3a469a36a897cc68fb87be3e8b3a5ba64f63a7904528408 dep5=BSD-3,MPL-2.0,Zlib,public-domain | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/nss lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-029 | apt | os-library | libpango-1.0-0 | 1.56.3-1 | snapshot:20261018T000000Z | debian-source pango1.0 1.56.3-1 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | LGPL-2+ and LGPL-2.1+ (Files: *); LGPL-2+ and TCL, LGPL-2+ and ICU, Unicode, Chromium-BSD-style, Example; Apache-2, Bitstream-Vera and OFL-1.1 for test fonts copyright-sha256=8d9dd932bc836ab2ea02e0b13c7cc1e9e5472a71008f0caa2a8793eb5780ee94 dep5=Apache-2,Apache-2_and_Bitstream-Vera_and_OFL-1.1,Bitstream-Vera,Chromium-BSD-style,Example,ICU,LGPL-2+,LGPL-2+_and_ICU,LGPL-2+_and_LGPL-2.1+,LGPL-2+_and_TCL,LGPL-2.1+,OFL-1.1,TCL,Unicode | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/pango1.0 lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-030 | apt | os-library | libudev1 | 257.13-1~deb13u1 | snapshot:20261018T000000Z | debian-source systemd 257.13-1~deb13u1 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | LGPL-2.1+ (Files: *); CC0-1.0, GPL-2 with Linux-syscall-note exception, Expat, GPL-2+, public-domain copyright-sha256=a79e4b379cabbd960bb44f6cc1a069081a3778c59a25ee529b68df5a2cb7d27d dep5=CC0-1.0,Expat,GPL-2+,GPL-2_with_Linux-syscall-note_exception,LGPL-2.1+,public-domain | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/systemd lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-031 | apt | os-library | libx11-6 | 2:1.8.12-1 | snapshot:20261018T000000Z | debian-source libx11 2:1.8.12-1 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | X11/MIT-style permission notices from Oracle, Open Group, X Consortium, XFree86, DEC, Fujitsu, OMRON, Sony, Wyse, Toshiba and a BSD-style notice (legacy text copyright; no SPDX names) copyright-sha256=0b380a7fd5b2228f26e9585e56f14812efd3350f3df307507d2bc055dfd8de3e dep5=none | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/libx11 lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-032 | apt | os-library | libxcb1 | 1.17.0-2+b1 | snapshot:20261018T000000Z | debian-source libxcb 1.17.0-2 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | MIT-style permission notice, Bart Massey, Jamey Sharp and Josh Triplett (COPYING; copyright built from debian/copyright.debian) copyright-sha256=4f7cb9db6bf6542f5417e3d674c780d3a5fd12291a54d63054fb576ee0cfae80 dep5=none | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/libxcb lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-033 | apt | os-library | libxcomposite1 | 1:0.4.6-1 | snapshot:20261018T000000Z | debian-source libxcomposite 1:0.4.6-1 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | Keith Packard permission notice plus an Oracle MIT-style notice (no SPDX names) copyright-sha256=e37c7a7e4026343c3e5c13990a6d9a8879194ae3ab26bfe420b34f6f206bf35f dep5=none | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/libxcomposite lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-034 | apt | os-library | libxdamage1 | 1:1.1.6-1+b2 | snapshot:20261018T000000Z | debian-source libxdamage 1:1.1.6-1 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | Keith Packard permission notice (no SPDX name) copyright-sha256=09182bc7805ff77a41251e450cebd0d33c48a74ba8ec4b734855a22c86f50417 dep5=none | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/libxdamage lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-035 | apt | os-library | libxext6 | 2:1.3.4-1+b3 | snapshot:20261018T000000Z | debian-source libxext 2:1.3.4-1 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | Open Group, DEC, SGI, NCD, HP and Oracle MIT/X11-style permission notices (no SPDX names) copyright-sha256=bc57e445ca1d9fe082c8d54189dd411ff26caa8552c9c63d44ea06a982f32124 dep5=none | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/libxext lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-036 | apt | os-library | libxfixes3 | 1:6.0.0-2+b4 | snapshot:20261018T000000Z | debian-source libxfixes 1:6.0.0-2 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | Keith Packard permission notice plus an Oracle MIT-style notice (no SPDX names) copyright-sha256=051ffe073ab38244c504bb379903b4ecda6081fb3d97d0d3dce44bc11712eef2 dep5=none | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/libxfixes lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-037 | apt | os-library | libxkbcommon0 | 1.7.0-2 | snapshot:20261018T000000Z | debian-source libxkbcommon 1.7.0-2 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | MIT-style permission notices (Open Group, Dan Nicholson) plus SGI and DEC permission-to-use notices (no SPDX name) copyright-sha256=5eeaeb1b6e029a0274e1573765bb0bae2926ef96a3679203faa4fd00fdaeaa88 dep5=none | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/libxkbcommon lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-038 | apt | os-library | libxrandr2 | 2:1.5.4-1+b3 | snapshot:20261018T000000Z | debian-source libxrandr 2:1.5.4-1 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | Compaq/HP and Keith Packard permission notices (no SPDX names) copyright-sha256=6d8e6239cfe9dd25bbeef06dcf0610d802ebbdc8968db87cf5adf00ff98927d4 dep5=none | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/libxrandr lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-039 | apt | direct-tool | qemu-user | 1:10.0.13+ds-0+deb13u1 | snapshot:20261018T000000Z | debian-source qemu 1:10.0.13+ds-0+deb13u1 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian (user-mode emulation) | GPL-2 (QEMU as a whole; no SPDX identifier in the file); libqemu core LGPL-2+; BSD emulator under a BSD licence; per-file notices copyright-sha256=7092076611fd6b8499ae39720ed86b7977a5dfa378038966d85d9987f7cfe784 dep5=none | upload ee492e23d8ae66376de7a301c7cb535df8228811 2026-08-29 https://metadata.ftp-master.debian.org/changelogs/main/q/qemu/qemu_10.0.13+ds-0+deb13u1_changelog work=qemu/1:10.0.13+ds-0+deb13u1 | linux/amd64 host; qemu-aarch64 and qemu-arm for arm64 and arm/v7 correctness | n/a |
| ADM-040 | apt | os-data | tzdata | 2026c-0+deb13u1 | snapshot:20261018T000000Z | debian-source tzdata 2026c-0+deb13u1 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | public-domain (IANA time zone database) copyright-sha256=cb61132bc0fc7b26ef5a82ee18b2fb644a1362f4f286ed980ff22e408471f59a dep5=public-domain | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/tzdata lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-041 | apt-fetch | direct-tool | unzip | 6.0-29+deb13u1 | snapshot:20261018T000000Z | debian-source unzip 6.0-29+deb13u1 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian (build stage only, absent from dev and work) | Info-ZIP license (version 2009-Jan-02; no SPDX identifier) copyright-sha256=aacc60ee90cc889820675362044cce73adf6bd97ed39cf847bb902ccdf6a4b63 dep5=none | upload 42334eabbc696974f21dfce2a68823e97a76a79c 2026-08-11 https://metadata.ftp-master.debian.org/changelogs/main/u/unzip/unzip_6.0-29+deb13u1_changelog work=unzip/6.0-29+deb13u1 | linux/amd64 | n/a |
| ADM-042 | apt | os-library | liblzma5 | 5.8.1-1+deb13u2 | snapshot:20261018T000000Z | debian-source xz-utils 5.8.1-1+deb13u2 http://snapshot.debian.org/archive/debian/20261018T000000Z/ | Debian | 0BSD (liblzma); the source copyright file also names LGPL-2.1+, GPL-2+, GPL-3+, public-domain and permissive notices copyright-sha256=d755588e2d89d7845d0d4e718ac45d49ae9fc53cec4540af105963c20598f9d7 dep5=,0BSD,FSFUL,FSFULLR,GPL-2+,GPL-3.0-or-later-WITH-Autoconf-exception-macro,LGPL-2.1+,PD,PD-debian,noderivs,none,permissive-nowarranty | distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/xz-utils lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear | linux/amd64 | n/a |
| ADM-100 | npm | npm-dependency | @axe-core/playwright | 4.13.0 | sha512-6YLx+kxXu5GJceG4ozFg+33a2EMTdjYwWGloJ3sb9Kta5pp+ZNS53uxGVog5JetIY8s++P5UrtX+cri+u0VAVg== | https://www.npmjs.com/package/@axe-core/playwright/v/4.13.0 | Deque Systems (npm maintainers dqlabs, wilcofiers, dylanb) | MPL-2.0 | release sha512-6YLx+kxXu5GJceG4ozFg+33a2EMTdjYwWGloJ3sb9Kta5pp+ZNS53uxGVog5JetIY8s++P5UrtX+cri+u0VAVg== 2026-08-11 https://www.npmjs.com/package/@axe-core/playwright/v/4.13.0 work=@axe-core/playwright/4.13.0 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-101 | npm | npm-dependency | @eslint/js | 10.0.1 | sha512-zeR9k5pd4gxjZ0abRoIaxdc7I3nDktoXZk2qOv9gCNWx3mVwEn32VRhyLaRsDiJjTs0xq/T8mfPtyuXu7GWBcA== | https://www.npmjs.com/package/@eslint/js/v/10.0.1 | OpenJS Foundation ESLint (npm openjsfoundation, eslintbot) | MIT | release sha512-zeR9k5pd4gxjZ0abRoIaxdc7I3nDktoXZk2qOv9gCNWx3mVwEn32VRhyLaRsDiJjTs0xq/T8mfPtyuXu7GWBcA== 2026-02-06 https://www.npmjs.com/package/@eslint/js/v/10.0.1 work=@eslint/js/10.0.1 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-102 | npm | npm-dependency | @inlang/paraglide-js | 2.26.0 | sha512-6k0WNzf941zTWtIbTHRP/Oq3Xg8LExUzpK1mteZ6HJAgRvIP8njh5tPtS4kO+mY/V+pQn88m16LEgLlWL7VteQ== | https://www.npmjs.com/package/@inlang/paraglide-js/v/2.26.0 | Opral (npm samuelstroschein, inlangbot) | MIT | release sha512-6k0WNzf941zTWtIbTHRP/Oq3Xg8LExUzpK1mteZ6HJAgRvIP8njh5tPtS4kO+mY/V+pQn88m16LEgLlWL7VteQ== 2026-10-06 https://www.npmjs.com/package/@inlang/paraglide-js/v/2.26.0 work=@inlang/paraglide-js/2.26.0 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-103 | npm | npm-dependency | @playwright/test | 1.64.0 | sha512-oTN9FNYuN6bqQMwiho6jJKLCG2/QOYCixGXuCjRmzOq23eODeukuY2XiAVoVpAECbuezMt1MHQjAiQxK8F+Y+g== | https://www.npmjs.com/package/@playwright/test/v/1.64.0 | Microsoft (npm pavelfeldman, yurys, dgozman-ms) | Apache-2.0 | release sha512-oTN9FNYuN6bqQMwiho6jJKLCG2/QOYCixGXuCjRmzOq23eODeukuY2XiAVoVpAECbuezMt1MHQjAiQxK8F+Y+g== 2026-10-07 https://www.npmjs.com/package/@playwright/test/v/1.64.0 work=@playwright/test/1.64.0 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-104 | npm | npm-dependency | @sveltejs/adapter-static | 4.0.0 | sha512-aF1J23R1ebKqUxQ4/qLr/Lxu63NFmLAtg8UZl3HFLxaoA1D/nhTVGcD9lTqx65iUbjNR2iOL0TgOxSX82x6Paw== | https://www.npmjs.com/package/@sveltejs/adapter-static/v/4.0.0 | Svelte (npm rich_harris, svelte-admin, conduitry) | MIT | release sha512-aF1J23R1ebKqUxQ4/qLr/Lxu63NFmLAtg8UZl3HFLxaoA1D/nhTVGcD9lTqx65iUbjNR2iOL0TgOxSX82x6Paw== 2026-10-01 https://www.npmjs.com/package/@sveltejs/adapter-static/v/4.0.0 work=@sveltejs/adapter-static/4.0.0 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-105 | npm | npm-dependency | @sveltejs/kit | 3.0.1 | sha512-8JMCCq7ZUJELi3hbhb0+c9Rs8qnbHW90vhOfM+t1FSF4fhIpG9/FOsH4IYgcITMhbdkSrrn+Kvd9G+q8A+UO6Q== | https://www.npmjs.com/package/@sveltejs/kit/v/3.0.1 | Svelte (npm rich_harris, svelte-admin, conduitry) | MIT | release sha512-8JMCCq7ZUJELi3hbhb0+c9Rs8qnbHW90vhOfM+t1FSF4fhIpG9/FOsH4IYgcITMhbdkSrrn+Kvd9G+q8A+UO6Q== 2026-10-06 https://www.npmjs.com/package/@sveltejs/kit/v/3.0.1 work=@sveltejs/kit/3.0.1 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-106 | npm | npm-dependency | @sveltejs/vite-plugin-svelte | 7.3.1 | sha512-ZPsLN8B1e/En+Ak5s4V7srFDT532oS0qieLsQwu63NGKsS+iAjoO2Js1BochlHlglcU+Pt7WAO3C5Ee+4f6gVA== | https://www.npmjs.com/package/@sveltejs/vite-plugin-svelte/v/7.3.1 | Svelte (npm rich_harris, svelte-admin, conduitry) | MIT | release sha512-ZPsLN8B1e/En+Ak5s4V7srFDT532oS0qieLsQwu63NGKsS+iAjoO2Js1BochlHlglcU+Pt7WAO3C5Ee+4f6gVA== 2026-09-23 https://www.npmjs.com/package/@sveltejs/vite-plugin-svelte/v/7.3.1 work=@sveltejs/vite-plugin-svelte/7.3.1 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-107 | npm | npm-dependency | @vitest/browser-playwright | 5.0.3 | sha512-rYBHwIFL/ZtI6dl0rbrAatScXCrvX+TL2BX2AGOigkC1Y6u3rIWw8KMQJJMS4drSa1ksMCmZU2wpgwdE7kqu3g== | https://www.npmjs.com/package/@vitest/browser-playwright/v/5.0.3 | Vitest (npm ariperkkio, antfu, hiogawa) | MIT | release sha512-rYBHwIFL/ZtI6dl0rbrAatScXCrvX+TL2BX2AGOigkC1Y6u3rIWw8KMQJJMS4drSa1ksMCmZU2wpgwdE7kqu3g== 2026-09-30 https://www.npmjs.com/package/@vitest/browser-playwright/v/5.0.3 work=@vitest/browser-playwright/5.0.3 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-108 | npm | npm-dependency | eslint | 10.12.0 | sha512-npHjrHb2o4ehH3mE+YuxABR+0gyb0aVWCIolgJDArwCiHEIsonBm3ZKjel5b0U5+a+MbEvIyRCGCzD3eKKK2NA== | https://www.npmjs.com/package/eslint/v/10.12.0 | OpenJS Foundation ESLint (npm openjsfoundation, eslintbot) | MIT | release sha512-npHjrHb2o4ehH3mE+YuxABR+0gyb0aVWCIolgJDArwCiHEIsonBm3ZKjel5b0U5+a+MbEvIyRCGCzD3eKKK2NA== 2026-10-02 https://www.npmjs.com/package/eslint/v/10.12.0 work=eslint/10.12.0 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-109 | npm | npm-dependency | eslint-plugin-svelte | 3.23.0 | sha512-n9jRklDqy0+W834568a4ZIZTK7DHXxvvHHxxGP8nNq7N//pOZMubttskHOquyGgfTR4Z05q2NdGNlsTpIEA48w== | https://www.npmjs.com/package/eslint-plugin-svelte/v/3.23.0 | Svelte (npm jounqin, conduitry, ota-meshi) | MIT | release sha512-n9jRklDqy0+W834568a4ZIZTK7DHXxvvHHxxGP8nNq7N//pOZMubttskHOquyGgfTR4Z05q2NdGNlsTpIEA48w== 2026-08-13 https://www.npmjs.com/package/eslint-plugin-svelte/v/3.23.0 work=eslint-plugin-svelte/3.23.0 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-110 | npm | npm-dependency | globals | 17.13.0 | sha512-RwMTC61u7hrG3ZJMkZQOK4JLJrKS8QtoTii63EmWvFXHqycY9FObBJQCbsLxSO8kjKhWE5kV1GC+Vb1g3RI0Zg== | https://www.npmjs.com/package/globals/v/17.13.0 | Sindre Sorhus (npm byk, nzakas, lo1tuma) | MIT | release sha512-RwMTC61u7hrG3ZJMkZQOK4JLJrKS8QtoTii63EmWvFXHqycY9FObBJQCbsLxSO8kjKhWE5kV1GC+Vb1g3RI0Zg== 2026-10-01 https://www.npmjs.com/package/globals/v/17.13.0 work=globals/17.13.0 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-111 | npm | npm-dependency | playwright | 1.64.0 | sha512-kVzTnYFYEyQ9RGzBjNCd2+yEqWmVbxl3AHPynv3qQ6JTjv4a+kwRcnqQwIpuP3NnrnIrE/Q6wWAIAvMfIE2/Yg== | https://www.npmjs.com/package/playwright/v/1.64.0 | Microsoft (npm pavelfeldman, yurys, microsoft1es) | Apache-2.0 | release sha512-kVzTnYFYEyQ9RGzBjNCd2+yEqWmVbxl3AHPynv3qQ6JTjv4a+kwRcnqQwIpuP3NnrnIrE/Q6wWAIAvMfIE2/Yg== 2026-10-07 https://www.npmjs.com/package/playwright/v/1.64.0 work=playwright/1.64.0 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-112 | npm | npm-dependency | prettier | 3.9.9 | sha512-Z/CJHIkdujO/OtN7nXUii0Rf3VT5SRuhjBA82Xvu2XhBUgX3nhP67T0LHceBdQLex7OOFGTox+Q5Yg8Jk2Qivg== | https://www.npmjs.com/package/prettier/v/3.9.9 | Prettier (npm jlongster, lydell, thorn0) | MIT | release sha512-Z/CJHIkdujO/OtN7nXUii0Rf3VT5SRuhjBA82Xvu2XhBUgX3nhP67T0LHceBdQLex7OOFGTox+Q5Yg8Jk2Qivg== 2026-09-23 https://www.npmjs.com/package/prettier/v/3.9.9 work=prettier/3.9.9 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-113 | npm | npm-dependency | prettier-plugin-svelte | 4.1.1 | sha512-wXvbXMjSvb4C9ENWTHXyd+ihakKCsJ6rJhLP6/8HFNj4GkZr48jqL9PoKsl2sk7SyCZRTnJ7O2TTowUpOxP/KA== | https://www.npmjs.com/package/prettier-plugin-svelte/v/4.1.1 | Svelte (npm conduitry, dummdidumm) | MIT | release sha512-wXvbXMjSvb4C9ENWTHXyd+ihakKCsJ6rJhLP6/8HFNj4GkZr48jqL9PoKsl2sk7SyCZRTnJ7O2TTowUpOxP/KA== 2026-06-15 https://www.npmjs.com/package/prettier-plugin-svelte/v/4.1.1 work=prettier-plugin-svelte/4.1.1 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-114 | npm | npm-dependency | sv | 1.1.1 | sha512-oacW2Ju3nY4wGTsGgMFmy7QbyuEuX6hKfqhOTWbUWJdeyScoJvvDYFCue2hfk8AUS1kJO+HGv+tvw16DSxmE8w== | https://www.npmjs.com/package/sv/v/1.1.1 | Svelte (npm rich_harris, svelte-admin) | MIT | release sha512-oacW2Ju3nY4wGTsGgMFmy7QbyuEuX6hKfqhOTWbUWJdeyScoJvvDYFCue2hfk8AUS1kJO+HGv+tvw16DSxmE8w== 2026-10-06 https://www.npmjs.com/package/sv/v/1.1.1 work=sv/1.1.1 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-115 | npm | npm-dependency | svelte | 5.57.2 | sha512-1YiPCAXT0Ct6g1vOMmSLa0Nsc3QRMvtDF32ihgglGVrugIuIK38RyDiFNXVDhtQ81TJjSO57NdMw6DyYLqSGDQ== | https://www.npmjs.com/package/svelte/v/5.57.2 | Svelte (npm rich_harris, conduitry, svelte-admin) | MIT | release sha512-1YiPCAXT0Ct6g1vOMmSLa0Nsc3QRMvtDF32ihgglGVrugIuIK38RyDiFNXVDhtQ81TJjSO57NdMw6DyYLqSGDQ== 2026-10-06 https://www.npmjs.com/package/svelte/v/5.57.2 work=svelte/5.57.2 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-116 | npm | npm-dependency | svelte-check | 4.7.6 | sha512-t2scM//ZuVbSY/T2w6FSBw1v9s2NEmh/g+sy1lqtosW5ylBV5AF4wFb1Ts9Kf3MbfPDUDJDZ9L436YT0SPTdvw== | https://www.npmjs.com/package/svelte-check/v/4.7.6 | Svelte (npm svelte-language-tools-deploy, dummdidumm) | MIT | release sha512-t2scM//ZuVbSY/T2w6FSBw1v9s2NEmh/g+sy1lqtosW5ylBV5AF4wFb1Ts9Kf3MbfPDUDJDZ9L436YT0SPTdvw== 2026-08-13 https://www.npmjs.com/package/svelte-check/v/4.7.6 work=svelte-check/4.7.6 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-117 | npm | npm-dependency | typescript | 6.0.3 | sha512-y2TvuxSZPDyQakkFRPZHKFm+KKVqIisdg9/CZwm9ftvKXLP8NRWj38/ODjNbr43SsoXqNuAisEf1GdCxqWcdBw== | https://www.npmjs.com/package/typescript/v/6.0.3 | Microsoft (npm microsoft1es, typescript-bot, weswigham) | Apache-2.0 | release sha512-y2TvuxSZPDyQakkFRPZHKFm+KKVqIisdg9/CZwm9ftvKXLP8NRWj38/ODjNbr43SsoXqNuAisEf1GdCxqWcdBw== 2026-04-16 https://www.npmjs.com/package/typescript/v/6.0.3 work=typescript/6.0.3 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-118 | npm | npm-dependency | typescript-eslint | 8.71.1 | sha512-oiNVPC3/NbV1FpaHU07l8O6ynqG9v4PAiW2WvTrAmZdR0f2HI/gE668ElMLCK7Xbsu5k2fK5C4fXNiq05j7/nQ== | https://www.npmjs.com/package/typescript-eslint/v/8.71.1 | typescript-eslint (npm jameshenry, bradzacher) | MIT | release sha512-oiNVPC3/NbV1FpaHU07l8O6ynqG9v4PAiW2WvTrAmZdR0f2HI/gE668ElMLCK7Xbsu5k2fK5C4fXNiq05j7/nQ== 2026-10-05 https://www.npmjs.com/package/typescript-eslint/v/8.71.1 work=typescript-eslint/8.71.1 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-119 | npm | npm-dependency | vite | 8.1.6 | sha512-aRAHIxx5SW5zc1sMqFwJU/t5qaFfXb5Hj0J7W0VXihu/X99+346tz6080tO1T1xVIJORX/b3zVTXGFqUXh35uw== | https://www.npmjs.com/package/vite/v/8.1.6 | VoidZero and Vite team (npm yyx990803, vitebot) | MIT | release sha512-aRAHIxx5SW5zc1sMqFwJU/t5qaFfXb5Hj0J7W0VXihu/X99+346tz6080tO1T1xVIJORX/b3zVTXGFqUXh35uw== 2026-10-06 https://www.npmjs.com/package/vite/v/8.1.6 work=vite/8.1.6 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-120 | npm | npm-dependency | vitest | 5.0.3 | sha512-xMw97S3rjdtj5dkVat7jCsqWBpvchs3RlpQctUqwJD0KkERk40vz2fJ77lDwW/Vzh/pk18eItYAzkodhSes3jQ== | https://www.npmjs.com/package/vitest/v/5.0.3 | Vitest (npm ariperkkio, antfu, hiogawa) | MIT | release sha512-xMw97S3rjdtj5dkVat7jCsqWBpvchs3RlpQctUqwJD0KkERk40vz2fJ77lDwW/Vzh/pk18eItYAzkodhSes3jQ== 2026-09-30 https://www.npmjs.com/package/vitest/v/5.0.3 work=vitest/5.0.3 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-121 | npm | npm-dependency | vitest-browser-svelte | 3.1.0 | sha512-+EBXSed6NArmzOC2DY4v9sI5n9TMIyfkMnewbXNQ7sR5IckQTCx7g1BbLdkH7kzm/tkgG/eEbRYlMWSbVfBtWA== | https://www.npmjs.com/package/vitest-browser-svelte/v/3.1.0 | vitest-community (npm ariperkkio, antfu, hiogawa) | MIT | release sha512-+EBXSed6NArmzOC2DY4v9sI5n9TMIyfkMnewbXNQ7sR5IckQTCx7g1BbLdkH7kzm/tkgG/eEbRYlMWSbVfBtWA== 2026-09-04 https://www.npmjs.com/package/vitest-browser-svelte/v/3.1.0 work=vitest-browser-svelte/3.1.0 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-122 | npm | npm-dependency | @inlang/plugin-message-format | 4.4.0 | sha512-n4aXt6XVg5kxhKoLAhi9nMgZtCA9iS0QOaXte56VqxWHcfj9O4c4gOkyVQZH7H9D8h7OZufCrO1sZGYOypPwEA== | https://www.npmjs.com/package/@inlang/plugin-message-format/v/4.4.0 | Opral (npm samuelstroschein, inlangbot; GitHub Actions trusted publisher; repository https://github.com/opral/inlang packages/plugins/inlang-message-format) | MIT (LICENSE file, Copyright (c) 2024 Opral US Inc.; the package.json has no license field) | release sha512-n4aXt6XVg5kxhKoLAhi9nMgZtCA9iS0QOaXte56VqxWHcfj9O4c4gOkyVQZH7H9D8h7OZufCrO1sZGYOypPwEA== 2026-03-13 https://www.npmjs.com/package/@inlang/plugin-message-format/v/4.4.0 work=@inlang/plugin-message-format/4.4.0 | any (JavaScript) | executed EVID-002 2026-10-16 |
| ADM-200 | go | go-dependency | github.com/dlclark/regexp2/v2 | v2.8.4 | h1:yfDDXTn4oxyLDC10/ieOBNDCCdIVWS9WbT6ddVXsPKI= | go-proxy https://proxy.golang.org origin=https://github.com/dlclark/regexp2 commit=e8c451ca75d22a6862c1e9b9f8f03175d20fcdda ref=refs/tags/v2.8.4 | Doug Clark (GitHub dlclark/regexp2) | MIT license-sha256=9be5d04bb4d706914d5bf943710da4afeb42048f7c529902fb57c82762a991a9 | commit e8c451ca75d22a6862c1e9b9f8f03175d20fcdda 2026-10-07 https://github.com/dlclark/regexp2/commit/e8c451ca75d22a6862c1e9b9f8f03175d20fcdda work=github.com/dlclark/regexp2/v2/v2.8.4 | pure Go, no cgo: linux/amd64 linux/arm64 linux/arm windows/amd64 windows/arm64 darwin/amd64 freebsd/amd64 freebsd/arm64 | n/a |

## Commitments (recorded, not installed)

| ID | commitment | detail |
| --- | --- | --- |
| CMT-TARGETS | eight Go compile targets (V-09) | linux/amd64 linux/arm64 linux/arm windows/amd64 windows/arm64 darwin/amd64 freebsd/amd64 freebsd/arm64 |
| CMT-PLATFORMS | three platforms for correctness (native plus QEMU) | linux/amd64 linux/arm64 linux/arm/v7 |
| CMT-IMAGES | three images | dev lock work (lock-export only publishes lock output) |
| CMT-P07 | explicit P-07 exception, replacement plan | vendoring openapi-fetch if it is archived; openapi-fetch is not introduced by this workspace |
| CMT-REGEXP2 | Go dependency of the parser slices | github.com/dlclark/regexp2/v2 v2.8.4, row ADM-200 (proxy release 2026-10-07, upstream commit e8c451ca75d22a6862c1e9b9f8f03175d20fcdda). It is in go.mod and in the `work` image's `/go/pkg/mod` cache with its first consumer. The row is admitted only by the real `--admission` run on the image the Go consumers used: `go list -m` under `GOPROXY=off`, `go mod verify`, and the module LICENSE hash and cache origin recomputed from that cache; the LICENSE hash recorded here was read from a go.sum-verified copy of the same release |
| CMT-BUN | runtime Bun compatibility of the eight web tools | executed EVID-002 2026-10-16 (SLICE-002): the root observed Bun 1.4.3 run all eight tools to `S5-TOOLS-PASS` (component tests 2/2, Playwright 5/5, axe violations 0, four captures validated); this records that run only, not feature completion or editor diagnostics |
| CMT-GOMOD | Go module directive | go 1.27, compiler Go 1.27.2; the linter must be built with a Go not older than this directive |

## Support-list block

The block below is immutable report data: the `security-support.deb13` release file of the Debian `debian-security-support`
project at the recorded commit, which root acquired from the `master` head of
https://salsa.debian.org/debian/debian-security-support (the merged current format; the pre-merge `-ended`/`-limited` files
never qualify). It carries no acquisition time and changes only when the upstream release file changes. The proof child
never fetches: `--admission` validates the block offline (one block, release equal to `/etc/os-release` `VERSION_ID`, URL
naming the same commit, SHA-256 of the fenced bytes, line grammar) and evaluates each OS row's installed source package
against it exactly as upstream `check-support-status` does (full-string PCRE match, `supported` overrides, `limited` and
dated `non-supported`). Root checks, immediately before and again immediately after every actual admission run, that the
upstream `master` head's release file still has this SHA-256 and records both pairs of observations in feature evidence.

support-list release=13 ref=master commit=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 sha256=d392b48a0d303551cddad04dbfa48d114738634d6107a1be5d25e563c13e5521 url=https://salsa.debian.org/debian/debian-security-support/-/raw/be1e8c21ab13b29cb831c7b5890d7420fe36cc56/security-support.deb13
```security-support.deb13
# List of packages whose security support is limited or ends before the distribution EOL

# File format: Columns, separated by one or more space characters
# 1. source package name
# 2. non-supported if security support ends completely, limited if it doesn't
#    benefit from the regular security support
# 3. last version with support
#    Important: If there have been binNMUs, enter the highest version
#    number used (only when non-supported)
# 4. Date when support ended or will end, in the form YYYY-mm-dd (only when non-supported)
# 5. Descriptive text or URL with more details (optional)
#    In the program's output, this is prefixed with "Details:"

adns                        limited  Stub resolver that should only be used with trusted recursors
binutils                    limited  Only suitable for trusted content; see https://lists.debian.org/msgid-search/87lfqsomtg.fsf@mid.deneb.enyo.de
cython                      limited  Only included for building packages, not running them, #975058
ganglia                     limited  See README.Debian.security, only supported behind an authenticated HTTP zone, #702775
ganglia-web                 limited  See README.Debian.security, only supported behind an authenticated HTTP zone, #702776
gnupg1                      limited  See #982258 and https://www.debian.org/releases/stretch/amd64/release-notes/ch-whats-new.en.html#modern-gnupg
gobgp                       limited  See https://www.debian.org/releases/trixie/release-notes/issues.en.html#go-and-rust-based-packages
golang.*                    limited  See https://www.debian.org/releases/trixie/release-notes/issues.en.html#go-and-rust-based-packages
hdf5                        limited  Not covered by security support, only suitable for trusted content, see #1117722
isc-dhcp                    non-supported   4.4.3-P1-2       2023-07-05  https://lists.isc.org/pipermail/dhcp-users/2022-October/022786.html
jython                      limited  Includes python2.7 stdlib, support limited until Py3 port, see #975058 and https://lists.debian.org/debian-lts/2024/08/msg00027.html
libsoup2.4                  limited  Only supported as a client, not as a server: see https://gitlab.gnome.org/GNOME/libsoup/-/commit/2a9d8ecc45bb814f6a81b1241e6c0c55d632aa28
libsoup3                    limited  Only supported as a client, not as a server: see https://gitlab.gnome.org/GNOME/libsoup/-/commit/2a9d8ecc45bb814f6a81b1241e6c0c55d632aa28
libspring-java              limited  See README.Debian.security included in the package
mozjs128                    limited  Not covered by security support, only suitable for trusted content, see package description
musescore2                  limited  Only supported with trusted files, see README.Debian shipped in package and #1070860
musescore3                  limited  Only supported with trusted files, see README.Debian shipped in package and #1070860
qt6-webengine               limited  No security support upstream and backports not feasible, only for use on trusted content
qtwebengine-opensource-src  limited  No security support upstream and backports not feasible, only for use on trusted content
rust.*                      limited  See https://www.debian.org/releases/trixie/release-notes/issues.en.html#go-and-rust-based-packages
rust-sequoia-sqv            supported  sequoia sqv is fully supported. See: https://bugs.debian.org/1106154
sql-ledger                  limited  Only supported behind an authenticated HTTP zone
tiles                       limited  Only supported for building packages, #1057343
vte                         limited  Not covered by security support, only used by debian-installer, #1082885
wpewebkit                   non-supported   2.38.6-1  2026-01-04  wpewebkit is generally unsupported from bookworm onwards, see #1118273, #1035997 and #1035794
zabbix                      limited  The WEB UI is only supported for access by trusted users, no security updates issued for it, #1124558
zoneminder                  limited  See README.Debian.security, only supported behind an authenticated HTTP zone, #922724
```

## Installed measurements and open root checks

1. Installed copyright SHA-256 and DEP-5 `License:` set for each Debian row ADM-010 to ADM-041 (32 rows) are recorded in the
   licence cells. They were measured on 2026-10-15 from the frozen dev image
   `sha256:1b898bb4bd5260017ced72f744ec9288e52b21dcc298450752a2534fc29697ef` (`/usr/share/doc/<package>/copyright`, resolved
   inside `/usr/share/doc`) and, for the build-only `unzip` row ADM-041, from the unzip copyright file the fetch stage retains
   (fetch-only unzip 6.0-29+deb13u1). For every row the installed version, `Section`, `${source:Package}` and
   `${source:Version}` equal the row's version, category and `debian-source` cell, so no row needed correction. Where a
   copyright file has no DEP-5 `License:` stanza the row keeps its plain-text licence names and records `dep5=none`.
   `--admission` recomputes both tokens from the installed files on every run and blocks a row with `licence_mismatch` on any
   difference; this measurement admits nothing by itself. To re-measure after an image change (host repository root):

   ```sh
   scripts/dev-run.sh -- scripts/s5-toolchain.sh --facts
   ```

   Each `FACT ADM-nnn ...` line gives the exact installed version, `Section`, `${source:Package}`, `${source:Version}`,
   `copyright-sha256` and `dep5` to compare with the row; a differing source or section is a row correction, never a waiver.
2. The root H2 pre-run and post-run checks of this block's SHA-256 against the upstream `master` head around each actual
   admission run, and the per-run `dev-run: image=... inputs=...` line with the immutable runtime payload recorded from it (see
   the runner notes below: ordered `RootFS.Layers`, `Config` SHA-256, platform tuple and `inputs=<sha>`), are root-owned
   procedure recorded in feature evidence only; no script or test in this repository stands in for them.

## Pin selection

- npm pins are the newest stable release inside the accepted families (Svelte 5, Kit 3, Vite 8, TypeScript 6, ESLint 10,
  Vitest 5) published at least seven days before 2026-10-15. TypeScript 7 and Kit 3.1.0 are newer but outside the accepted
  family or younger than seven days. Peer ranges were read from registry metadata (Kit 3.0.1 needs Vite ^8.0.12, Svelte
  ^5.57.1, TypeScript ^6, vite-plugin-svelte ^7; typescript-eslint 8.71.1 accepts TypeScript below 6.1).
- Playwright 1.64.0 `browsers.json` maps chromium-headless-shell revision 1248 to Chrome for Testing 156.0.8078.4, the
  archive ADM-006 pins. With no `channel`, Playwright's `getExecutableName` resolves a headless launch to
  chromium-headless-shell, and Vitest 5.0.3 passes `browser.headless` through without a channel, so no full Chrome archive is
  installed, inventoried or admitted (there is no ADM-007 row and no `chromium-<revision>` directory). Vitest otherwise
  defaults to headed outside CI, so SLICE-002's Vitest config sets `browser.headless: true` and its Playwright config sets
  `headless: true`; neither sets `channel` or `executablePath`. The Docker build places the shell as
  `chromium_headless_shell-1248` with `INSTALLATION_COMPLETE`, without any JavaScript runtime.
- golangci-lint 2.14.0: its linux/amd64 binary embeds Go 1.27.0 build information, which is why `go.mod` says `go 1.27`.
  Admission executes the linter against a planted printf defect and a clean fixture, so an incompatible build still blocks.
- Debian packages are pinned by exact version inside one dated snapshot of trixie, trixie-updates and trixie-security;
  transitive packages resolve deterministically from that snapshot, and the Dockerfile's only apt suites are those three
  (component main). The dev image admits linux/amd64 only and fails the build on any other architecture.

## Cold bootstrap and lock generation (owner Docker and network required)

`web/bun.lock` is generated by the admitted Bun inside the `lock` image, never by a host tool or by hand, and the `work`
build fails closed without it. From the repository root:

```sh
docker build -f deploy/dev/Dockerfile --target lock-export --build-arg LOCK_ECOSYSTEM=web --output type=local,dest=web .
```

That publishes exactly `web/bun.lock`. A Go lock (`go.sum`/`go.mod`, once Go dependencies exist) is exported separately with
`--build-arg LOCK_ECOSYSTEM=go --output type=local,dest=.`; each export contains only its own ecosystem's files. Every
`scripts/dev-run.sh` run afterwards rebuilds `work` from current allowlisted inputs with `bun install --frozen-lockfile`
and never regenerates either lock. First the small runner check (it must print `DEV-RUN-SMOKE-PASS checks=7`), then V-01:

```sh
scripts/dev-run-smoke.sh && BOXSET_SECRET_SENTINEL=synthetic-not-a-credential scripts/dev-run.sh --write docs/spikes/issue-drafts -- scripts/s5-toolchain.sh --admission
```

A failing admission keeps its exit status and leaves a bounded redacted draft at `docs/spikes/issue-drafts/s5-admission.md`
for owner review; nothing is filed externally.

## Runner notes

- `scripts/dev-run.sh [--write docs/spikes/issue-drafts] [--capture DIR] [--timeout SECONDS] -- COMMAND [ARG...]` is one small POSIX
  `sh` wrapper around Docker, run from the repository root. `--bench` stops with `bench_unavailable` until the work that
  introduces it lands (`--capture` with `--bench` is `capture_flag_invalid`), and any other `--write` path is
  `write_path_invalid`; `--capture DIR` is described in the capture
  note below. The timeout is 1 to 600 seconds (default 600). Host Go, Bun and Node are never
  used: every command, including this report's admission, runs in the `work` image.
- Each run rebuilds `work` from the current allowlisted inputs with frozen locks, creates one uniquely named, labelled
  container and runs COMMAND with its exact argv as the fixed user 1000:1000 (whatever the host UID), `--network none`,
  `--cap-drop ALL`, `--security-opt no-new-privileges`, no published port, no socket and no mount. The child environment is
  cleared and holds only the literal names PATH, HOME, TMPDIR, LANG, LC_ALL, GOPATH, GOCACHE, GOMODCACHE, GOFLAGS, GOPROXY,
  GOSUMDB, GOTOOLCHAIN, GOTELEMETRY, GOENV, GOLANGCI_LINT_CACHE, BUN_INSTALL_CACHE_DIR, BUN_RUNTIME_TRANSPILER_CACHE_PATH,
  XDG_CACHE_HOME, PLAYWRIGHT_BROWSERS_PATH, PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD and DO_NOT_TRACK (plus the shell's own
  PWD/OLDPWD/SHLVL/_ and Docker's HOSTNAME). No value comes from the host environment: every Docker call (both builds, the
  create, the inspections and the cleanup) runs with a private, empty Docker client configuration on the daemon the caller
  selected (another daemon stops the run with `docker_unavailable`), so no owner proxy or credential setting is injected, and
  before the start the runner reads the created container's environment back and stops with `env_invalid` unless every line is
  one of the literals above or the image's own, unchanged. No `.env` or owner secret is in the
  image context (`.dockerignore` is deny-by-default and re-excludes nested `.env`, `.env.*`, `*.pem`, `*.key`, `.npmrc` and
  `.netrc`; the four host-only scripts `scripts/dev-run.sh`, `scripts/dev-run-smoke.sh`, `scripts/dev-run-hardening.sh` and
  `scripts/dev-run-test.sh` each have their own exclusion line and are named by the admission's exposure check).
- The container's daemon log is disabled (`--log-driver=none`); before the start the runner requires
  `docker inspect --format '{{.HostConfig.LogConfig.Type}}'` to print `none` (otherwise `log_driver_invalid`). Its
  `dev-run: image=<id> inputs=<sha> timeout=<n>s container=<name> log=none` line on stderr identifies the run (the created
  container has no mount of any kind, else `mounts_invalid`). The image ID names
  one build only: a Go consumer run is bound to an actual admission run by the immutable runtime payload, recorded by root's
  read-only `docker image inspect` of that run's `image=<id>` while the image exists: the complete ordered `RootFS.Layers`
  digest chain, the SHA-256 of the parsed `Config` value alone (`json.dumps(Config, ensure_ascii=True, sort_keys=True,
  separators=(",", ":"))` encoded as UTF-8, lowercase hexadecimal; never the whole inspect record, and raw Config JSON is never
  printed or stored), the `Os`/`Architecture`/`Variant` platform tuple (`Variant` is the empty string when absent) and the
  runner's `inputs=<sha>`. Any differing, unavailable, malformed or ambiguous component needs its own admission run; equal source
  or input hashes alone never bind. Output is read
  only from the attached stream, never from `docker logs`, merged and passed through the host redactor (`deploy/dev/redact`,
  `-cap 65536`, secret values taken from the runner's own environment), so no raw child byte is retained anywhere. The
  redactor is built by the existing `host-redact` stage into a unique, ownership-checked scratch directory the runner removes.
- Exit status: the child's status, read from Docker's terminal container state and required to agree with the numeric status of
  the attached `docker start` (a missing, malformed or disagreeing status is `status_unknown`; only a fired host deadline may
  disagree and then stays `124`, even when the child afterwards exits 0), or `125` with a fixed reason word on stderr (for
  example `usage`, `timeout_invalid`, `write_path_invalid`, `redact_unavailable`, `build_failed`, `output_overflow`,
  `status_unknown`, `env_invalid`, `export_failed`, `export_invalid`, `draft_overflow`, `cleanup_failed`, `publish_failed`).
  Output beyond 64 KiB stops the run with `output_overflow`. `scripts/s5-toolchain.sh --root DIR` takes the physical path, so
  `/src/.` is the real root and runs every control; any other root is a manifest-only copy that prints
  `S5-MANIFEST-ONLY-PASS` and is never admission.
- `--write docs/spikes/issue-drafts` publishes only the fixed names `s5-admission.md` and `s5-tools.md` that the child left
  under `/src/docs/spikes/issue-drafts`: after the container stopped, each exported as one tar stream (at most 256 KiB read)
  that must hold exactly one regular file of at most 64 KiB raw (a link, directory, device, second entry or other type is
  `export_invalid`, a larger file `draft_overflow`), streamed through the host redactor and capped at 64 KiB emitted; raw
  bytes only cross pipes and are never written to the scratch directory; neither are the archive's member names (tar's listing
  is read through a pipe of at most 4 KiB and only a fixed one-entry token is kept) nor `docker cp`'s raw diagnostics (at most
  4 KiB raw are read; a pipe-side `sed` keeps only the fixed token `M` when a line says the file was not found, before the
  capped redactor, so a selected secret value that occurs in that message cannot turn an absent optional draft into a failure;
  the redacted remainder is what is kept). The statuses of `docker cp`, `tar`, the redactor, the diagnostic reader, classifier and
  redactor are each checked, and publication happens only after the owned container was removed by a checked `docker rm -f`. A draft is
  published even when the child exits nonzero. Overflow, an unknown status or a failed export, removal or publication
  publishes nothing further and exits 125; a later rename failure can leave earlier names already published, so rerun. There
  is no rollback and no crash-durability claim, and a draft is never filed externally.
- Recovery of a failing bootstrap. Before the redactor exists the runner prints only a fixed stage reason and exits 125, never a
  raw build diagnostic: `redact_unavailable` (the `host-redact` build, export or check failed) or `temp_dir_failed` (the
  private scratch directory). Confirm that the Docker daemon is running and that the registry and snapshot network the pinned
  stages need is reachable, then run exactly `scripts/dev-run.sh -- true` and, when it exits 0, V-01 above. A failing `work`
  build afterwards shows only its redacted tail. There is no clock or controller step.
- The small runner check `scripts/dev-run-smoke.sh` archives the explicit admitted roots (with the final nested secret and
  cache exclusions of `.dockerignore` applied before archiving, so no owner-shaped nested file is copied) into its own unique,
  ownership-checked scratch directory (so repository drafts are never touched), runs that copy's runner through real Docker
  and prints `ok R<n>` or `not ok R<n>` for seven assertions: R1 argument errors before Docker (including `--ref` as an unknown
  flag, `usage`, and `--write testdata/parser` as `write_path_invalid`), R2 exact argv and child status
  and a child outliving `--timeout 1` giving 124 with no container left, R3 child identity, environment names (no `/ref`
  directory and no unlisted name), network and
  `log=none` under a synthetic Docker proxy configuration (disposable assertions in the scratch Dockerfile's redactor build and
  work stage, under a fresh nonce, show the configuration reaches neither build, and stop a direct build given it), R4 a
  synthetic secret printed by the child is `[REDACTED]`, R5 the same secret and a secret URL written into a draft by a failing
  child are published redacted with no part file and no container left, an oversized draft, a draft whose redaction expands
  past the cap, a link and a directory holding a secret-named entry fail the export with the prior draft intact, a passive
  wrapper of the real Docker scanning only the check's own scratch after each `docker rm` sees the runner's scratch and no
  synthetic secret in any file or name there, a selected value `file` (which occurs in docker's missing-file message) still
  publishes a lone draft from a failing child (the retained copy diagnostic must hold the redaction marker and not the value),
  lets a silent child with no drafts return 0 with the prior draft intact, and publishes a lone `s5-tools.md` with the child's
  status when the first name is absent, a synthetic tree with nested secret-shaped entries snapshots without them, R6
  the existing redaction-only Go tests through the runner with `CGO_ENABLED=0`
  (the selector lists only `main_test.go` tests), and R7 the root alias `/src/.` running the real
  controls (an unexpected child variable blocks it), a small noncanonical manifest copy printing only `S5-MANIFEST-ONLY-PASS`
  and no runtime control, the real admission blocking an impossible executed Bun date (`recorded Bun result date ... is not a
  valid date`) and a stale annual evidence date (the latter with a retained redacted draft), then the Go negatives: the go
  row's licence SHA-256, origin commit and `h1:` pin each mutated in its own real-root run (`licence_mismatch`,
  `source_mismatch`, `go.sum lacks ...`) and a manifest-only copy with one extra `go.mod` directive and one `require` without a
  row (`ADMISSION-UNLISTED`), none printing `S5-ADMISSION-PASS`.
  `DEV-RUN-SMOKE-PASS checks=7` prints only when all seven pass.
- `--capture DIR` (DIR is `.devrites/work/<slug>/screenshots-YYYYMMDDTHHMMSSZ` under an existing workspace directory and must
  not exist; a bad shape is `capture_destination_invalid`, an existing entry `capture_destination_exists`, both before Docker,
  and an existing DIR is never reused or deleted) adds exactly two synthetic variables to the child, `BOXSET_CAPTURE_DIR=/capture`
  and a fresh 16-hex `BOXSET_CAPTURE_CHALLENGE`. The `work` image holds one ordinary private `/capture` (1000:1000, 0700); no
  mount, tmpfs or supervisor exists. Only after the child exited 0 and its container stopped does the runner take the four fixed
  files `{dev,preview}-{desktop,mobile}.png` out of it, each streamed straight into its own fresh validator container (the
  installed `capture-check`, user 1000:1000, 64 MiB memory, no network, read-only root, 30 s host timeout). The validator is
  created first and the runner reads back `HostConfig.LogConfig.Type=none` for exactly that container before it is ever started
  (else `log_driver_invalid`; a validator created with another driver never starts and no capture is published), printing one
  `dev-run: validator=<name> log=none container=<name>` line, four in all (the producer's own `log=none` line proves none of
  them). Raw capture bytes only cross pipes and are never written on the host: `tar` admits exactly one regular non-link entry
  of the fixed name and at most 2,097,152 bytes and hands it to the validator's standard input (a missing file is
  `capture_missing`, an oversize entry `capture_oversize`, any other entry or a failed copy `capture_copy_failed`). `docker cp`
  diagnostics are bounded, classified by a pipe-side `sed` (only the fixed missing-file token is kept) and then redacted and
  capped exactly like the draft exporter's; the validator's own diagnostics are discarded and only its exit word is used
  (`capture_missing` for an empty input, `capture_not_png`, `capture_not_static`, `capture_dimensions`,
  `capture_challenge_mismatch`). Only the validator's canonical PNG is kept, through a bounded sink whose own status is checked
  apart from the validator's and Docker's; every validator and then the child are removed by a checked `docker rm -f` before
  anything is published. `capture.manifest` (`boxset-capture 1`, the challenge, then `<sha256> <bytes> <name>.png` per file)
  and the four PNGs are published as five 0644 files in a DIR the runner creates 0700 and removes again if publication fails,
  followed by `DEV-RUN-CAPTURE-PASS files=4 bytes=<n> dir=<DIR> challenge=<hex>`. A nonzero child keeps its status and
  publishes no capture.
- `scripts/s5-toolchain.sh --tools` (run as `scripts/dev-run.sh --write docs/spikes/issue-drafts --capture DIR -- scripts/s5-toolchain.sh --tools`)
  and `scripts/dev-run-smoke.sh --tools-failure` are described in `docs/spikes/s5-bun-toolchain.md`. The npm rows' `bun` column
  reads `executed EVID-002 2026-10-16`: the root's own `--tools` run of 2026-10-16 (eight tools on Bun 1.4.3, all passing) was
  recorded in feature evidence; a report row is never marked executed from this text or from a leaf run, and an impossible
  executed date (for example 2026-02-30) is blocked as `recorded Bun result date ... is not a valid date`.
- The `@inlang/plugin-message-format` 4.4.0 manifest and lock entries were generated by the admitted Bun, never by hand and
  never by the runner: a one-shot network-enabled container from the `dev` target ran
  `bun add --dev --exact --ignore-scripts @inlang/plugin-message-format@4.4.0` on an ownership-checked scratch copy of
  `web/package.json` and `web/bun.lock`, and the resulting bytes were copied back unedited (the lock gained only the direct
  entry and the two resolved records for the plugin and its declared dependency `flat` 6.0.1). The frozen `work` image install
  still acquires dependencies with that lock; every proof child, including the Paraglide compilation, stays `--network none`.

## Deferred optional runner hardening (preserved predecessor)

Deferred, optional and not part of the active flow above. The notes below describe `scripts/dev-run-hardening.sh`, a
byte-for-byte copy of the pre-revision `scripts/dev-run.sh` (changed since only by the owner's neutral-name correction; the native-controller runner, with `scripts/dev-run-test.sh` as its
driver and `redact -run`/`-clock` in `deploy/dev/redact`), not the active runner; inside these preserved notes
`scripts/dev-run.sh` names that pre-revision script. Nothing here is required by a gate or a check, none of it has been proven,
and its historical findings stay open.

- `scripts/dev-run.sh [--write PATH] [--capture DIR] [--bench CPUSET] [--timeout SECONDS] -- COMMAND [ARG...]` runs
  from the repository root. The child is the fixed user 1000:1000 (whatever the host UID); PID 1 and every supervisor exec
  run as 65534 on root-owned absolute binaries that a trust preflight checks before the child starts. Child output (stdout
  and stderr merged) passes through the host redactor `deploy/dev/redact` and is capped at 64 KiB raw and 64 KiB emitted;
  build and start diagnostics appear only as a redacted tail.
- `--write testdata/parser` and `--write docs/spikes/issue-drafts` are supervisor-owned protected fixed-name maps on a
  32 MiB tmpfs (`series.jsonl`/`movie.jsonl`, `s5-admission.md`/`s5-tools.md`): the child writes the precreated files in
  place, the kernel refuses the byte past 33,554,432, and nothing reaches the host until the child is quiescent
  (only terminal UID-1000 zombies may remain), the container is checked closed and staged parts are published by rename.
  Drafts are counted raw before redaction and emitted redacted separately. Publication commits only while every backup
  exists; a post-commit backup-cleanup failure is reported and never rolls back. The live cap is enforced during
  production, so the earlier post-run-only limitation no longer applies.
- Capture mode follows architecture DEC-020 and the strict enforcement fold: a protected precreated four-file `/capture`
  tmpfs map, host-exec child, fresh-container `capture-check` validation, and root-supplied fresh workspace DIR; the
  guarantees need root runtime proof and are not claimed here.
- Native lifecycle controller. `scripts/dev-run.sh` is only a bootstrap: it parses and validates the arguments, audits what it can without
  touching anything, builds the runner-private host helper (`deploy/dev/redact`, one static stdlib binary: the redactor and the controller),
  checks it and then `exec`s `redact -run`. The controller keeps the runner's PID, environment, cwd, umask and stdio and is the only
  lifetime authority: it alone starts native processes, holds the issued names, the sticky create and removal failures and the cancellation
  state in memory, runs closure, decides publication and exits; no file, child exit status or pipe content grants or revokes that authority,
  and no shell survives the exec. A name is issued if and only if the native process of its `docker run` started, a failed, timed-out or
  unknown create is retained, and closure observes exactly the issued names. HUP, INT and TERM only latch a cancellation (HUP and INT unless
  inherited ignored): a create is waited to its own bound, reconciliation always completes, every other call is killed with its process group,
  and the run ends 125 `interrupted`; SIGKILL, an untrapped signal or a Go fatal error is host loss, bounded by the container's own expiry.
  The one shared 15 s reconciliation window is read as exact CLOCK_MONOTONIC nanoseconds when the removal step ends, each confirmation is
  bounded by the time left floored to milliseconds, a completion must be strictly before the window ends, and a failed, malformed, backward,
  invalid or late reading or answer is terminal. A relative `PATH` entry resolves against the original repository directory (an
  `exec.ErrDot` result is a found executable), and a relative `TMPDIR` is resolved by the bootstrap before anything is created.
- Recovery of a failing bootstrap. `scripts/dev-run.sh -- true` prints the existing redacted bootstrap diagnostics (a failing helper build, a
  rejected helper check or an unusable clock reading is `redact_unavailable`, a missing run-private directory `temp_dir_failed`). The driver
  acquires the same helper once for its `redact -clock` reader; if that fails it prints `dev-run-test: clock unavailable phase=acquire` or
  `phase=probe`, exits 125 and credits nothing. Run the command above, fix the reported step and rerun; the clock checks are never bypassed and
  there is no fallback clock.
- Deferred, still open with labelled final Review ownership: D-L01 (the query argv stays TERM-only; the hard native-call bound including I/O
  completion is enforced by the controller instead of the shell's wrappers), D-L02 (the capture transfer budget keeps whole wall seconds + 30),
  L03 (the driver fixture is untouched) and R4-M11 (the redaction engine is reused with one immutable selection; its combined read and write
  error precedence is not claimed byte-unchanged).
- `scripts/dev-run-test.sh` credits every RED and GREEN (runner, validator, redactor, S5 and named-Go controls, plain
  controls and the host-loss results) through one classifier: the intended target must have been reached after every
  prerequisite succeeded, setup, infrastructure and unknown observations must be clear, no earlier assertion of the same case may have
  failed, and the assertion must be refused (RED on the mutant) or held (GREEN on the original or fault copy); anything else fails. Plain
  controls are credited only as a GREEN through the same gate (`pass`), and their target is never "the runner spoke": it is the runner's
  identity line printed just before the child starts or, for a control that expects a refusal, that exact fixed word. An
  intended infrastructure refusal (a failing build, a missing host tool) exempts only its own line from infrastructure
  attribution and needs its injected step's own observation; a controller panic or Go fatal trace is infrastructure whatever its status
  coincides with; only the classifier's own meta-tests are exempt. An unknown Docker observation is recorded from the parent shell. Every
  runner mutant and fault copy is a compiled copy of the helper package built in a private context (a copy that does not compile is
  infrastructure, never RED); only bootstrap controls are shell copies. The deterministic regression for that classifier, its case latch,
  the plain-control targets, the clock reader and its recovery diagnostic, the lint-target shapes, the host-loss result classes, the readiness
  probes (only an exact `present` or `absent` answer is an observation), the host-loss bounds (the monotonic window [S+D, R+D] from the
  logging shim's own run record and Docker's running witness, no wall clock), the actual watcher and the bounded terminal helper driven on a
  virtual clock, the natural-cell premise over the receipts of the logging Docker shim and the four-record supervisor hash oracle needs no
  container and no build: `scripts/dev-run-test.sh --classifier-self-check` (exit 0, `DEV-RUN-TEST-SELFCHECK-PASS`); the default run starts
  with the same check, then the full real-Docker proof. A passing self-check is not an admission result.
- Driver fixtures state their premises independently of the rejecting target: the non-dumpable control reads the live
  UID-1000 task's own `/proc/<pid>/task/<tid>/status` owner (root) and Uid line (1000 1000) while the child is held; every
  cancellation cell (pre, gap, inflight, group, nativegrp, startfail at the container and each validator) runs through the logging shim with
  compiled barriers at real controller statements and waits for the controller's own `dev-run: interrupted` line before releasing a barrier;
  inherited signal dispositions are established before exec and read back from the controller's `SigIgn` mask; the delayed and
  effect-before-error create controls run the real `docker run` and receipt its status and output before the
  final absence is read, the removal controls receipt the real `docker rm -f` before the injected failure, and driver
  removal is never counted as runner closure; a natural delayed-effect control perturbs the create seam only, requires
  every exact-name query before the effect to have exactly one well-formed shim receipt with status 0, an empty answer and a completion at
  least 10 ms before the effect began, and reads its terminal state inside one absolute budget in which every query is bounded by what is left.
  Each transfer-fault seam, S5 report fixture, scanner section, linter control (its own `CONTROL` line or the exact planted-defect block, no
  unrelated blocked row) and named Go assertion has its own target receipt, never a generic refusal word or generic admission completion.
  Host-loss results are monotonic: absence counts only inside the case's own window, survival only to a query that started at or after the
  deadline, and an unobserved final fraction is unknown. The closure clock boundary, every fractional grace, a zero remaining budget at a
  query admission and a clock failure at the loop-continuation reading are scripted-reader cases through the real closure, never real-time
  scheduling. Stalled-validator and host-loss controls exercise the validator's native 10 s + 1 s expiry; the delayed-validator control uses
  the original validator arguments, isolation and stdin and is not expiry evidence. The stalled-validator control starts both its 30 s transfer
  bound and its 20 s validator-lifetime bound at the logging shim's own reading before the validator's `docker run`, not at a later first
  observation, and a variant that removes only the in-container timeout is a separate mutant from the one that removes every backstop.
- Controller behaviour changed by the correction of the first source review (each with its own control, all NOT-RUN): the diagnostic stream is
  written by one worker through a byte-bounded queue (a blocked or full stderr loses text beyond the bound, never the failure state or the exit
  status, and the exit flush waits at most 1 s); the final capture locator is written under a 5 s bound and a failed or blocked delivery is
  `output_write_failed` (125) without rolling back the committed capture; the removal of the owned container after an output failure runs under
  the same wait as its parent call, so the parent's deadline and cancellation stay serviced while it runs; cancellation is checked after each
  accounted backup, between renames, before the commit point and as the last step of the capture publication, and nothing rolls back after a
  commit point; a backup is this run's only when its link was made here and the name still holds that exact inode, in every rollback and
  cleanup branch (an absent target beside a pre-existing backup name, or a backup replaced by a foreign file, is left byte- and inode-exact and a
  replaced one makes publication fail or report cleanup failure); an unusable closure
  clock ends the confirmation phase for every outstanding name. The argv0 of every native call is the constant `timeout`; the only
  caller-chosen words (the COMMAND) run inside the container.
- Driver controls added by that correction: every plain run declares its intended final status (the explicit status, 125 with a refusal word,
  else 0) and `rg_verdict` refuses every credit of the case while the real status differs; the draft, relative-PATH, signal-disposition,
  acquisition-transition, tamper, host-canary and publication-signal controls are reached only by a receipt of their own seam; deferred
  host-loss cases carry the controller-fatal taint like the ordinary ones; the host-loss rows record the compiled controller sha and the
  original controller sha; the clock mutation rows of the self-check and every `clock-reads-selection` and `clock-rounds` named-Go copy compare the exact
  wrong outcome they promise, never an arbitrary inequality.
- Provider-test fixtures (`deploy/dev/redact`) state their premises before the observation they gate: the malformed run-private directory case
  executes a surviving real binary and still expects the exact refusal with no filesystem effect; a killed group is dead when its grandchild
  is gone or a zombie by its own `/proc/<pid>/stat` state (the container's PID 1 never reaps an adopted zombie) and any other state or a failed
  read is never absence; the escaped pipe holder records its pid only after leaving the shell's process group, and the test reads it alive, alone
  in its own session and holding the very pipe the call created before the 2 s bound is observed and again after it; HUP and INT cancellation
  run in real re-executed processes whose dispositions are set before exec (`env --default-signal` or `--ignore-signal`) and read back from
  `SigIgn`, with a TERM latch as the positive control of the ignored cell and nothing skipped.
- Owner-recheck correction (all NOT-RUN): the fatal-guard self-check sources the extracted real `hl_err_taint` in its isolated scope before any
  perturbation (a helper that is missing, unreadable, malformed or undefined completes no outcome, so its rows fail), and the ordinary
  fatal-guard mutant removes the whole rejection, both the `rerr_seen` record and the independent `infra_failure` scan, so the exact wrong
  outcome it promises is reachable; after an inherited-ignored signal has positively cancelled the target (owner exited or interrupted line
  printed) the release of its already removed container is not demanded, while any other failed release stays setup; the closed-locator
  fixture closes the FIFO read/write descriptor in the runner's child scope and establishes with a non-blocking write open that no reader
  remains before the final-announcement barrier is released; the invalid-socket transport probes run with `HOME=/nonexistent`.
- Proof status: every control above is written and NOT-RUN after its last correction. The last observed runs before the owner-recheck
  correction were: syntax pass, classifier self-check 2 failed of 118 (`hl_err_taint: not found`, ordinary fatal mutant missed), provider test
  top-level PASS, four-image capture pass; no canonical full proof, attributable pair, final admission or freshness rehearsal has been
  executed, and root runs them once after the source is frozen. Nothing here is a runtime acceptance claim; D-L01, D-L02, L03 and R4-M11 stay
  open as labelled above.
- `scripts/s5-toolchain.sh --tools`, `scripts/dev-run-test.sh --tools-failure` and `--benchmark-evidence` are owned by
  SLICE-002 and SLICE-007 and fail with a fixed message until those slices extend them.

## Sources (retained dossiers and primary records, observed 2026-10-15 unless noted)

- Debian image: `docker buildx imagetools inspect debian:trixie-slim` (index digest, creation annotation 2026-10-05);
  official-images commit 6b287dc80f463f088b8d6432ceeb633ec0c0b8a4 (references/annual-tool-evidence-20261015.md).
- Debian packages: `Packages.xz` of trixie, trixie-updates and debian-security trixie-security at snapshot 20261018T000000Z;
  binary-to-source and Section from references/apt-evidence-first-20261015.md, apt-evidence-second-20261015.md and
  debian-support-evidence-20261015.md; copyright names from the per-source files on metadata.ftp-master.debian.org.
- Debian support: https://www.debian.org/releases/ (Debian 13 full support through 2028-08-09); salsa
  `debian-security-support` `security-support.deb13` at commit be1e8c21ab13b29cb831c7b5890d7420fe36cc56 and its
  `check-support-status.in` (references/support-current-bytes-20261015.md); per-source tracker pages
  `https://security-tracker.debian.org/tracker/source-package/<source>`.
- Go: https://go.dev/dl/?mode=json (archive checksum) and https://go.dev/doc/devel/release (go1.27.2 released 2026-10-08).
- Bun: GitHub release bun-v1.4.3 `SHASUMS256.txt`, LICENSE.md (MIT with LGPL-2 JavaScriptCore).
- golangci-lint: GitHub release v2.14.0 `golangci-lint-2.14.0-checksums.txt` (published 2026-09-24).
- Chrome for Testing: archive downloaded from storage.googleapis.com, SHA-256 computed locally; GCS generation
  1790731396923999; Playwright 1.64.0 `browsers.json` revision 1248 and Playwright/Vitest launch-path source
  (references/browser-artifact-evidence-20261015.md).
- gcc-defaults 1.233 changelog (2026-08-02) as the in-window upload of the same source package as the installed
  gcc-defaults 1.220 pin (references/annual-tool-evidence-20261015.md).
- npm: registry.npmjs.org package documents (`dist.integrity`, `time`, `license`, `maintainers`, peer ranges).
- Playwright download paths and Debian 13 dependency list: `playwright-core@1.64.1` package (`coreBundle.js`).
