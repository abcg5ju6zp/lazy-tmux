FROM public.ecr.aws/x8v8d7g8/mars-base:latest
WORKDIR /app
ARG REPO_URL=https://github.com/alchemmist/lazy-tmux
ARG BASE_SHA=e71dced983f29f1f438502983c9dc3945b6c52d7
RUN git clone "$REPO_URL" . \
 && DEFAULT="$(git remote show origin | sed -n 's/.*HEAD branch: //p')" \
 && git checkout -B "$DEFAULT" "$BASE_SHA" \
 && git remote remove origin \
 && git reflog expire --expire=now --all \
 && git gc --prune=now
RUN go mod download
CMD ["bash"]
