# Haunt branding

The product name is **Haunt**. The owner-supplied brand kit is preserved under
`client/branding/haunt/`, including its source artwork, specification, original
tokens and Android exports. The supplied launcher artwork is preserved. A separate white SVG owl, requested
by the owner for the notification button, is derived from that artwork; it does
not replace the original kit exports. `client/scripts/check-brand-assets.mjs` verifies the source
logo and 27 installed image exports against the kit's supplied hashes on every
client build.

The client uses neutral charcoal surfaces (#161616, #202020, #292929, #333333),
with equal RGB channels and no warm or cool tint. Orange remains the default
action accent and can be replaced in Appearance. The Inter-first font stack
uses semibold names and regular supporting text. Status colors retain their meanings: speaking and
connection indicators stay green, and audio meters retain green/orange/red
levels. Uploaded server/user artwork, transparent backgrounds, custom server
names and configured accent colors remain user content. No slogans were added.

## Asset locations

| Purpose | Location |
| --- | --- |
| Original specification and artwork | `client/branding/haunt/` |
| Served logos, favicons, touch icons and manifest | `client/apps/web/public/brand/` |
| White notification vector | `client/static/notification-mark.svg`, served from `public/brand/` |
| Applied theme tokens | `client/apps/web/src/brand-tokens.css` |
| Shared web asset paths and product name | `client/packages/browser/src/brand.ts` |
| Windows, macOS and Linux launcher icons | `client/static/` |

The web manifest is served beneath `/brand/`; its start URL, ID and scope resolve
to the application root. Unread favicon badges use the approved owl with a small
orange indicator. The decoded logo is cached once, and asynchronous badge work
is invalidated when its subscription is disposed.

Electron's visible product name, window/about/menu labels and installer names are
Haunt. The builder app ID is `com.exampleuser.haunt`; Linux/Windows executables are
named `haunt`. The separate `electron-app/` staging directory contains built UI,
the runtime manifest, Electron shell modules and the launcher icon. The
original brand kit and development dependencies are excluded from the ASAR.

## Compatibility identities

Existing Go module paths, npm package names, `HAUNT_*` environment variables,
database/schema names, wire protocols, backup formats, IPC channels and storage
keys remain stable. The desktop origin remains `https://app.chatxo.local` and its
persistent Chromium partition remains `persist:chatxo`.

Desktop application and notification names are Haunt. Packaged profiles use
`haunt`; development profiles use `haunt-development`. On startup, an existing
legacy profile is moved before the updater and browser open it. Saved sessions
are re-encrypted using Haunt's keychain identity, with plaintext confined to a
private process pipe. Migration failures retain the encrypted data and report
an error; conflicting nonempty profiles are never merged or overwritten.
Explicit `--user-data-dir` overrides retain their location, and a `haunt`
directory override can migrate its legacy sibling.

New server configurations default to Haunt. Explicitly configured server names
and uploaded artwork remain under their operators' control.

The supplied Android artwork is retained for future packaging. The notification
glyph uses white paths and transparent negative space. Banner names use a
translucent neutral backing and backdrop blur for readability.
