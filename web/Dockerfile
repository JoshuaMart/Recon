FROM node:24-alpine AS build

WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

FROM node:24-alpine

WORKDIR /app
COPY --from=build /app/.output .output
COPY --from=build /app/node_modules/simple-icons ./node_modules/simple-icons
COPY --from=build /app/node_modules/devicon ./node_modules/devicon

EXPOSE 3000
CMD ["node", ".output/server/index.mjs"]
