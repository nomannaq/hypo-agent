import { router, usePage } from '@inertiajs/react';
import { CalendarClock, LogOut, Plus, Settings, Sparkles } from 'lucide-react';
import type { ReactNode } from 'react';
import { AISidebar } from '@/components/agents/ai-sidebar';
import type { SidebarResource } from '@/components/agents/ai-sidebar';
import { ChatApp } from '@/components/agents/chat-app';
import AppLogo from '@/components/app-logo';
import {
    AnimatedSidebar,
    AnimatedSidebarContent,
    AnimatedSidebarFooter,
    AnimatedSidebarGroup,
    AnimatedSidebarGroupLabel,
    AnimatedSidebarHeader,
    AnimatedSidebarInset,
    AnimatedSidebarMenu,
    AnimatedSidebarMenuButton,
    AnimatedSidebarMenuItem,
    AnimatedSidebarTrigger,
} from '@/components/motion/animated-sidebar';
import { agent, agentChat, logout, newAgentChat, schedules } from '@/routes';
import { edit as editProfile } from '@/routes/profile';
import type { BreadcrumbItem } from '@/types';

const RESOURCES: SidebarResource[] = [
    { id: 'hypotheses', label: 'Hypotheses', kind: 'project', children: [] },
];

export default function AgentLayout({
    breadcrumbs = [],
    children,
}: {
    breadcrumbs?: BreadcrumbItem[];
    children: ReactNode;
}) {
    const { auth, name, conversations, conversationID } = usePage<{
        conversations?: { id: string; title: string }[];
        conversationID?: string;
    }>().props;
    const { url } = usePage();
    const title = breadcrumbs.at(-1)?.title ?? name;

    return (
        <ChatApp className="h-svh rounded-none border-0">
            <AnimatedSidebar ariaLabel="Workspace">
                <AnimatedSidebarHeader>
                    <div className="flex items-center gap-2 px-2 py-1.5">
                        <AppLogo />
                    </div>
                </AnimatedSidebarHeader>
                <AnimatedSidebarContent>
                    <AnimatedSidebarGroup>
                        <AnimatedSidebarMenu>
                            <AnimatedSidebarMenuItem>
                                <AnimatedSidebarMenuButton
                                    icon={<Plus />}
                                    onSelect={() =>
                                        router.visit(newAgentChat())
                                    }
                                >
                                    New chat
                                </AnimatedSidebarMenuButton>
                            </AnimatedSidebarMenuItem>
                            <AnimatedSidebarMenuItem>
                                <AnimatedSidebarMenuButton
                                    icon={<Sparkles />}
                                    isActive={url.startsWith(agent().url)}
                                    onSelect={() => router.visit(agent().url)}
                                >
                                    Agent
                                </AnimatedSidebarMenuButton>
                            </AnimatedSidebarMenuItem>
                            <AnimatedSidebarMenuItem>
                                <AnimatedSidebarMenuButton
                                    icon={<CalendarClock />}
                                    isActive={url.startsWith(schedules.url())}
                                    onSelect={() =>
                                        router.visit(schedules.url())
                                    }
                                >
                                    Schedules
                                </AnimatedSidebarMenuButton>
                            </AnimatedSidebarMenuItem>
                        </AnimatedSidebarMenu>
                    </AnimatedSidebarGroup>
                    {!!conversations?.length && (
                        <AnimatedSidebarGroup>
                            <AnimatedSidebarGroupLabel>
                                Chats
                            </AnimatedSidebarGroupLabel>
                            <AnimatedSidebarMenu>
                                {conversations.map((chat) => (
                                    <AnimatedSidebarMenuItem key={chat.id}>
                                        <AnimatedSidebarMenuButton
                                            isActive={
                                                conversationID === chat.id
                                            }
                                            onSelect={() =>
                                                router.visit(agentChat(chat.id))
                                            }
                                        >
                                            <span className="truncate">
                                                {chat.title}
                                            </span>
                                        </AnimatedSidebarMenuButton>
                                    </AnimatedSidebarMenuItem>
                                ))}
                            </AnimatedSidebarMenu>
                        </AnimatedSidebarGroup>
                    )}
                    <AnimatedSidebarGroup>
                        <AnimatedSidebarGroupLabel>
                            Projects
                        </AnimatedSidebarGroupLabel>
                        <AISidebar
                            defaultItems={RESOURCES}
                            ariaLabel="Projects"
                        />
                    </AnimatedSidebarGroup>
                </AnimatedSidebarContent>
                <AnimatedSidebarFooter>
                    <AnimatedSidebarMenu>
                        <AnimatedSidebarMenuItem>
                            <AnimatedSidebarMenuButton
                                icon={<Settings />}
                                isActive={url.startsWith('/settings')}
                                onSelect={() => router.visit(editProfile().url)}
                            >
                                {auth.user?.name ?? 'Settings'}
                            </AnimatedSidebarMenuButton>
                        </AnimatedSidebarMenuItem>
                        <AnimatedSidebarMenuItem>
                            <AnimatedSidebarMenuButton
                                icon={<LogOut />}
                                onSelect={() => router.post(logout().url)}
                            >
                                Log out
                            </AnimatedSidebarMenuButton>
                        </AnimatedSidebarMenuItem>
                    </AnimatedSidebarMenu>
                </AnimatedSidebarFooter>
            </AnimatedSidebar>
            <AnimatedSidebarInset className="flex min-h-0 flex-col">
                <header className="flex h-12 shrink-0 items-center gap-2 px-3">
                    <AnimatedSidebarTrigger />
                    <span className="text-sm font-medium">{title}</span>
                </header>
                <div className="min-h-0 flex-1 overflow-y-auto">{children}</div>
            </AnimatedSidebarInset>
        </ChatApp>
    );
}
