# Step 1: Builds and tests
FROM node:16.20.2-bullseye AS build

ARG kubeflowversion
ARG commit

ENV BUILD_VERSION=$kubeflowversion
ENV BUILD_COMMIT=$commit
ENV CHROME_BIN=/usr/bin/chromium
ENV PUPPETEER_SKIP_CHROMIUM_DOWNLOAD=true

<<<<<<< HEAD
RUN apt update -qq && apt install -qq -y gnulib

COPY . /centraldashboard
WORKDIR /centraldashboard

RUN BUILDARCH="$(dpkg --print-architecture)" &&  npm rebuild && \
    if [ "$BUILDARCH" = "arm64" ]  ||  \
    [ "$BUILDARCH" = "armhf" ]; then \
    export CFLAGS=-Wno-error && \
    export CXXFLAGS=-Wno-error;  \
    fi && \
    npm install && \
    npm run build && \
    npm prune --production
=======
COPY . /centraldashboard
WORKDIR /centraldashboard

RUN npm ci \
 && npm run build \
 && npm prune --production
>>>>>>> 48b8643bee14b8c85c3de9f6d129752bb55b44d3

# Step 2: Packages assets for serving
FROM node:16.20.2-alpine AS serve

RUN apk add --no-cache tini

USER node

ENV NODE_ENV=production
WORKDIR /usr/src/app
COPY --from=build --chown=node:node /centraldashboard .

EXPOSE 8082
ENTRYPOINT ["/sbin/tini", "--" , "npm", "start"]
