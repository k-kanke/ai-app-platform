FROM node:20-alpine
ARG INSTALL_CLAUDE=false
ARG INSTALL_GEMINI=false
RUN apk add --no-cache bash curl git tini coreutils \
 && if [ "$INSTALL_CLAUDE" = "true" ]; then npm install -g @anthropic-ai/claude-code; fi \
 && if [ "$INSTALL_GEMINI" = "true" ]; then npm install -g @google/gemini-cli@0.62.0; fi
COPY entrypoint.sh /opt/aap/entrypoint.sh
COPY AGENTS.md /opt/aap/AGENTS.md
COPY agents /opt/aap/agents
COPY templates /opt/aap/templates
RUN chmod +x /opt/aap/entrypoint.sh /opt/aap/agents/*.sh
ENV HOME=/tmp/home AAP_AGENT=template GEMINI_CLI_TRUST_WORKSPACE=true
USER 1000:1000
WORKDIR /workspace
ENTRYPOINT ["/sbin/tini", "--", "/opt/aap/entrypoint.sh"]
