# Changelog

## [0.0.2](https://github.com/orglane/orglane/compare/v0.0.1...v0.0.2) (2026-11-07)


### Features

* **agent:** agent process, signed socket protocol, reconciliation and core client ([#73](https://github.com/orglane/orglane/issues/73)) ([8c414d9](https://github.com/orglane/orglane/commit/8c414d96a9cf3a974a0cf77893e2ea9e8565737f))
* **apps:** drive the agent from the core with apply and sync jobs ([#87](https://github.com/orglane/orglane/issues/87)) ([4253251](https://github.com/orglane/orglane/commit/4253251853f07422d76ebe04f92bbbdd80d0fba0))
* **apps:** provider framework in the core ([#74](https://github.com/orglane/orglane/issues/74)) ([4b4403a](https://github.com/orglane/orglane/commit/4b4403a5d69581a9e91052ad551efe005ab76189))
* **deploy:** ship the agent in the bundle with an enable-managed installer ([#76](https://github.com/orglane/orglane/issues/76)) ([85096a7](https://github.com/orglane/orglane/commit/85096a7a4fcd4a2b487a2a71b6334e28050277dc))
* **forgejo:** managed package, lifecycle adapter and dynamic ingress routes ([#85](https://github.com/orglane/orglane/issues/85)) ([7980987](https://github.com/orglane/orglane/commit/798098748ff697f4e13a4f83bb154b0a716653e8))
* **github:** connect organizations with a GitHub App ([#80](https://github.com/orglane/orglane/issues/80)) ([6779282](https://github.com/orglane/orglane/commit/67792823e63a0b008ddd1a1c4a821bb360dbfe8d))
* **people:** expire import runs and erase source material on apply ([#54](https://github.com/orglane/orglane/issues/54)) ([0165ca1](https://github.com/orglane/orglane/commit/0165ca15fa5b0a7aa69adfabfdc5dfb8a7183293))


### Bug Fixes

* **apps:** keep removal follow-ups, honor RetryAfter, never swallow a setup retry ([#86](https://github.com/orglane/orglane/issues/86)) ([b5718ff](https://github.com/orglane/orglane/commit/b5718ff460ce82e6b525060aca9ede32ec61378b))
* **deploy:** name an edge subnet clash and capture the compose status in the installer ([#49](https://github.com/orglane/orglane/issues/49)) ([464e12c](https://github.com/orglane/orglane/commit/464e12c98f40f9516cb2078df4eca1ec8b3375bc))
* **identity:** answer 409 to an invitation accepted during the lookup ([#55](https://github.com/orglane/orglane/issues/55)) ([1a87f02](https://github.com/orglane/orglane/commit/1a87f024e1b17936b7867e7c950bca2bba560a84))
* **oidc:** answer a revocation without a token with invalid_request ([#56](https://github.com/orglane/orglane/issues/56)) ([33e3e5c](https://github.com/orglane/orglane/commit/33e3e5cc762794983dfc8a148ac5d76921926e69))
* **oidc:** decrypt tokens with authenticated AES-256-GCM only ([#50](https://github.com/orglane/orglane/issues/50)) ([c879a28](https://github.com/orglane/orglane/commit/c879a28999a02fdf4a9d71e7afe4f3109c6a8ccf))
* **website:** reset copy button label when a copy fails after a success ([#47](https://github.com/orglane/orglane/issues/47)) ([c2a7cdb](https://github.com/orglane/orglane/commit/c2a7cdbd653dfbf281c3e45dba7fa5975094be36))

## 0.0.1 (2026-11-04)


### Features

* **core:** add core skeleton with server, agent and cli modes ([51eeb13](https://github.com/orglane/orglane/commit/51eeb1368ed20c04b6b997cbd979b8a67ef574a0))
* **core:** add M0 data model with audited identity service ([be729fe](https://github.com/orglane/orglane/commit/be729fe799c9bf9bc9ce0d1d37f7bd445ae6199e))
* **demo:** add demo mode with the Acme Robotics dataset ([9b36210](https://github.com/orglane/orglane/commit/9b362103d7515aa62e4df35b38e65087aedca542))
* **deploy:** add the one-command installer ([9e9a106](https://github.com/orglane/orglane/commit/9e9a1064c423a60538f60efbc0d026bb0d1c1474))
* **deploy:** add Traefik and Compose bundle with routes served by the core ([5451afa](https://github.com/orglane/orglane/commit/5451afaf87160e1e3d287fdc7204521d03dd8383))
* **identity:** add embedded OIDC provider and sign-in ([0b65be0](https://github.com/orglane/orglane/commit/0b65be0106e0281604602dc918c5861aa1a88482))
* **identity:** add system email and invitations ([#36](https://github.com/orglane/orglane/issues/36)) ([787a872](https://github.com/orglane/orglane/commit/787a872733e8b35d6dad548948b7ef6d85bb9ab0))
* **packages:** add generated TypeScript API client ([7698108](https://github.com/orglane/orglane/commit/7698108de6eeaa16052449cee97e05fe9ae16557))
* **people:** add directory and profile pages ([#37](https://github.com/orglane/orglane/issues/37)) ([97036d0](https://github.com/orglane/orglane/commit/97036d09a41a898b3f484852260faaf1b5031c6b))
* **people:** add onboarding checklist and welcome flow ([#39](https://github.com/orglane/orglane/issues/39)) ([c99c872](https://github.com/orglane/orglane/commit/c99c872583c00fe8d71ca5391c859b04309b1510))
* **people:** add profile fields, library, and privacy controls ([#35](https://github.com/orglane/orglane/issues/35)) ([c003d77](https://github.com/orglane/orglane/commit/c003d7700076e4f175842bb34a165dcb8651bbf3))
* **people:** add staged CSV and XLSX import ([#41](https://github.com/orglane/orglane/issues/41)) ([5eebae5](https://github.com/orglane/orglane/commit/5eebae50a6d9ebe30b1df299579a984a4e976922))
* **people:** add teams and memberships ([#34](https://github.com/orglane/orglane/issues/34)) ([dc487a0](https://github.com/orglane/orglane/commit/dc487a0912a78295a57c976e5ad78a8ba84d2033))
* **people:** import GitHub organization members ([#43](https://github.com/orglane/orglane/issues/43)) ([88c6fca](https://github.com/orglane/orglane/commit/88c6fcac1621cde8866213cfbfb587339de68e7f))
* **web:** add web skeleton embedded into the core binary ([9fe67d5](https://github.com/orglane/orglane/commit/9fe67d573fe02af63438e9d1a4d3af3563ed275a))
* **website:** add the orglane.app landing page serving install.sh ([ec18378](https://github.com/orglane/orglane/commit/ec1837856aa5581699712d0863b71df6e7a86e7d))


### Bug Fixes

* **identity:** require a session-bound CSRF token on the logout confirmation ([3bbfd6d](https://github.com/orglane/orglane/commit/3bbfd6d0fe85e72b72ca255f53b1e6e5bc6d09a4))
* **oidc:** revoke refresh tokens at the revocation endpoint; test audit rollback on database failures ([a5d52c9](https://github.com/orglane/orglane/commit/a5d52c9a2695d8a5275e8f8f0e92605bf9c192ce))


### Documentation

* mark M0 and M1 done; number pre-alpha releases 0.0.x ([#44](https://github.com/orglane/orglane/issues/44)) ([eb07dbb](https://github.com/orglane/orglane/commit/eb07dbba4252b07e55f37aed574e6a2a1600c7a3))
