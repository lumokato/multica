"use client";

import { useMemo, useState } from "react";
import { ChevronDown, ChevronRight, ClipboardList } from "lucide-react";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@multica/ui/components/ui/collapsible";
import type { ChatMessage } from "@multica/core/types";
import { RichContent } from "../../rich-content";
import { useT } from "../../i18n";

const OUTPUT_MARKER = "**执行体输出**";
const END_MARKER = "⟦任务结果结束⟧";

/**
 * Splits the server-authored result card into its three visible parts: the
 * self-describing header (issue key, status, producing agent), the status/table
 * block, and the run output. The card owns no task and is display-only, so the
 * output folds away by default instead of competing with the conversation.
 */
export function splitTaskResultContent(content: string) {
  const markerIndex = content.indexOf(OUTPUT_MARKER);
  const head = markerIndex >= 0 ? content.slice(0, markerIndex) : content;
  const output =
    markerIndex >= 0
      ? content
          .slice(markerIndex + OUTPUT_MARKER.length)
          .replace(END_MARKER, "")
          .trim()
      : "";
  const [firstLine = "", ...rest] = head.split("\n");
  const title = firstLine
    .replace(/^⟦任务结果⟧\s*/, "")
    .replace(/^📋\s*/, "")
    .trim();
  return { title, body: rest.join("\n").trim(), output };
}

/**
 * One sub-issue result delivered into the conversation (LS-206). Visually
 * distinct from the agent's own reply so the member can tell a card produced by
 * a sub-task from something this session's agent said. The server header line
 * already names the issue, status and producing agent; the card only adds the
 * "not this conversation" framing and the fold.
 */
export function TaskResultCard({ message }: { message: ChatMessage }) {
  const { t } = useT("chat");
  const [outputOpen, setOutputOpen] = useState(false);
  const { title, body, output } = useMemo(
    () => splitTaskResultContent(message.content),
    [message.content],
  );

  return (
    <div
      className="w-full rounded-xl border border-dashed border-border bg-muted/30 px-3.5 py-3"
      data-testid="task-result-card"
      data-message-kind={message.message_kind}
    >
      <div className="flex items-center gap-1.5 text-caption text-muted-foreground">
        <ClipboardList className="size-3.5 shrink-0" aria-hidden />
        <span className="font-medium text-foreground">
          {t(($) => $.message_list.task_result_badge)}
        </span>
        <span>·</span>
        <span>{t(($) => $.message_list.task_result_caption)}</span>
      </div>
      {title ? (
        <div className="mt-1.5 text-body font-medium break-words">{title}</div>
      ) : null}
      {body ? (
        <RichContent
          content={body}
          density="compact"
          phase="settled"
          className="mt-1.5 text-body"
        />
      ) : null}
      {output ? (
        <Collapsible open={outputOpen} onOpenChange={setOutputOpen}>
          <CollapsibleTrigger
            className="mt-2 flex items-center gap-1 text-caption text-muted-foreground transition-colors hover:text-foreground"
          >
            {outputOpen ? (
              <ChevronDown className="size-3.5" aria-hidden />
            ) : (
              <ChevronRight className="size-3.5" aria-hidden />
            )}
            {t(($) => $.message_list.task_result_output)}
          </CollapsibleTrigger>
          <CollapsibleContent>
            <RichContent
              content={output}
              density="compact"
              phase="settled"
              className="mt-1.5 text-body"
            />
          </CollapsibleContent>
        </Collapsible>
      ) : null}
    </div>
  );
}
