# The console. A Dockerfile of its own rather than a stage of the Go one,
# because nothing is shared: a different runtime, a different package manager,
# and a dependency tree with no overlap.

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

# Only what the node adapter serves, and it is genuinely all of it: the adapter
# bundles what the server needs into build/, so there is no node_modules here.
# Every dependency of this project is a devDependency, so a prune would have
# left an empty directory anyway, and `pnpm prune` aborts without a TTY, which
# is how that was noticed.
#
# The sources, the tooling and the tests stay in the build stage.
COPY --from=build /app/build ./build
COPY --from=build /app/package.json ./

# Never root. The console holds a session cookie and renders titles and headers
# collected from hostile targets; it has no reason to be able to write anywhere.
USER node

EXPOSE 3000

# ORIGIN is deliberately not defaulted. Without it the node adapter refuses
# every legitimate POST, and the console refuses to start rather than letting
# that appear at the first form. A default here would be a default everybody
# inherits and nobody notices being wrong.
CMD ["node", "build/index.js"]
