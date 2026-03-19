FROM node:24-alpine AS build

WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

FROM node:24-alpine

WORKDIR /app
COPY --from=build /app/.output .output

EXPOSE 3000
CMD ["node", ".output/server/index.mjs"]
