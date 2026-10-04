# Generic App Runtime (Stage 1): runs the source mounted at /app.
# App Contract: PORT, DATA_DIR=/data, GET /healthz, `npm start`.
FROM node:20-alpine
RUN apk add --no-cache tini
ENV HOME=/tmp NPM_CONFIG_CACHE=/tmp/.npm NODE_ENV=production
USER 1000:1000
WORKDIR /app
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["npm", "start"]
