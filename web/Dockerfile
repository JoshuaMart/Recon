# The console. Its own Dockerfile, not a stage of the Go one: nothing is shared (different
# runtime, package manager and dependency tree).

FROM node:22-alpine AS build
WORKDIR /app
RUN corepack enable

# The manifest and the lockfile first, so this layer is rebuilt only when the
# dependencies move.
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

COPY web/ ./
RUN pnpm run build

FROM node:22-alpine
WORKDIR /app
ENV NODE_ENV=production

# Only what the node adapter serves, which is all of it: the adapter bundles what the server
# needs into build/, so there is no node_modules here. Every dependency is a devDependency, so
# a prune would leave an empty directory anyway, and `pnpm prune` aborts without a TTY, which
# is how that was noticed.
#
# Sources, tooling and tests stay in the build stage.
COPY --from=build /app/build ./build
COPY --from=build /app/package.json ./

# Never root. The console holds a session cookie and renders titles and headers
# collected from hostile targets; it has no reason to be able to write anywhere.
USER node

EXPOSE 3000

# ORIGIN is deliberately not defaulted. Without it the node adapter refuses every legitimate
# POST, and the console refuses to start instead of showing it at the first form. A default
# would be inherited by everyone and noticed wrong by nobody.
CMD ["node", "build/index.js"]
