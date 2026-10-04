FROM node:20-alpine
ARG INSTALL_CLAUDE=false
RUN apk add --no-cache bash curl git tini \
 && if [ "$INSTALL_CLAUDE" = "true" ]; then npm install -g @anthropic-ai/claude-code; fi
COPY entrypoint.sh /opt/aap/entrypoint.sh
COPY AGENTS.md /opt/aap/AGENTS.md
COPY agents /opt/aap/agents
COPY templates /opt/aap/templates
RUN chmod +x /opt/aap/entrypoint.sh /opt/aap/agents/*.sh
ENV HOME=/tmp/home AAP_AGENT=template
USER 1000:1000
WORKDIR /workspace
ENTRYPOINT ["/sbin/tini", "--", "/opt/aap/entrypoint.sh"]
