// English strings of the website. Every user-facing string lives in a
// catalog; templates only compose them. Another language is another catalog
// of the same shape (`Messages`), registered in ./index.ts.

export const en = {
  meta: {
    title: "Orglane — the open-source operating system for your company",
    description:
      "People, access, tools and notifications in one app, built on proven open-source services. Self-hosted, free and open source. Pre-alpha.",
    notFoundTitle: "Page not found — Orglane",
  },
  nav: {
    label: "Main",
    skip: "Skip to content",
    home: "Orglane home",
    features: "Features",
    install: "Install",
    github: "GitHub",
  },
  hero: {
    badge: "Pre-alpha",
    badgeDetail: "Early and experimental, not for real company data yet",
    title: "The open-source operating system for your company",
    lead: "People, access, tools and notifications in one app, built on proven open-source services. For companies from 3 people to 300.",
    leadSecondary: "Run it on your own server, free and open source.",
    install: "Install",
    github: "View on GitHub",
  },
  stack: {
    label: "How Orglane fits together",
    clients: "Mobile and web app",
    clientsDetail: "One app for the whole company",
    api: "One company API",
    apiDetail: "Modules and AI agents, with scopes and MCP",
    core: "Orglane core",
    coreDetail: "People · identity · permissions · events · audit",
    providers: "Your services",
    providersDetail: "Forgejo, GitHub, Telegram, Google Calendar, …",
  },
  features: {
    eyebrow: "What we are building",
    title: "One ecosystem instead of a pile of tools",
    intro:
      "Big companies give their people one internal app. Orglane brings that to every company, without the ops burden of running a dozen self-hosted apps.",
    planned: "This is the plan for the first developer preview (v0.1); none of it is ready yet.",
    roadmap: "Follow each milestone on the roadmap",
    plannedTag: "Planned",
    items: [
      {
        title: "One app",
        body: "Mobile and web. Sign in with your work email or scan a QR code, and the whole company is there: people, teams, notifications and every service.",
      },
      {
        title: "One identity",
        body: "Hire someone once and they get the right access everywhere. Offboard once and access is revoked everywhere, with a report from each service.",
      },
      {
        title: "Real integration, not a list of links",
        body: "People and teams are provisioned into each service, their events become one inbox, and everything opens with single sign-on.",
      },
      {
        title: "Self-hosting without the pain",
        body: "Tested version sets, upgrades with automatic snapshots and rollback, and backups that are verified by restoring them.",
      },
      {
        title: "Open to modules and AI agents",
        body: "One API with explicit scopes, a consent screen and an audit log, exposed over MCP from the start. No SSO tax, no artificial limits.",
      },
    ],
  },
  install: {
    title: "Install",
    intro:
      "One command on Linux or macOS with Docker. It checks the machine, asks how you want to run Orglane and prints the address to open.",
    todayTitle: "What you get today",
    today:
      "Sign-in with passwords and passkeys, an embedded OpenID Connect provider, people and roles, and the first screens of the web app, plus the Acme Robotics demo company. Integrations, notifications, the mobile app and backups come next.",
    noticeTitle: "Pre-alpha software",
    notice:
      "Orglane is not ready for real company data. Expect missing features, breaking changes and no upgrade path between early versions.",
    commandLabel: "Install Orglane",
    demoLabel: "Or try the demo company on a laptop, without questions",
    copy: "Copy",
    copied: "Copied",
    copyFailed: "Couldn’t copy. The command is selected: copy it by hand.",
    copyCommand: "Copy the command",
    integrityTitle: "Check the script first",
    integrity:
      "Read the installer before you run it, or download it and check its SHA-256 checksum.",
    readScript: "Read install.sh",
    checksumLabel: "SHA-256 of install.sh",
    verifyLabel: "Download and check the checksum",
    verifyResult: "It prints “install.sh: OK” when the download matches, and fails otherwise.",
    verifyMacos: "On macOS, use shasum -a 256 -c instead of sha256sum -c.",
  },
  footer: {
    tagline: "The open-source operating system for your company.",
    linksLabel: "Project",
    github: "GitHub",
    roadmap: "Roadmap",
    security: "Security policy",
    trademark: "Trademark policy",
    license:
      "The server and web UI are licensed under AGPL-3.0-or-later; the apps, SDKs and shared packages under Apache-2.0.",
    trademarkNote:
      "“Orglane” and the Orglane logo are trademarks of the project and are not covered by the code licenses.",
    privacy: "This site sets no cookies and loads nothing from other sites.",
    copyright: "© The Orglane contributors",
  },
  notFound: {
    title: "Page not found",
    body: "There is nothing at this address yet.",
    home: "Go to the home page",
  },
};

export type Messages = typeof en;
