import { Link } from '@inertiajs/react';
import {
    CalendarClock,
    ChevronRight,
    Folder,
    FolderOpen,
    MessageSquare,
    Network,
    Plug,
    SquarePen,
} from 'lucide-react';
import type { LucideIcon } from 'lucide-react';
import AppLogo from '@/components/app-logo';
import { NavUser } from '@/components/nav-user';
import {
    Collapsible,
    CollapsibleContent,
    CollapsibleTrigger,
} from '@/components/ui/collapsible';
import {
    Sidebar,
    SidebarContent,
    SidebarFooter,
    SidebarGroup,
    SidebarGroupLabel,
    SidebarHeader,
    SidebarMenu,
    SidebarMenuButton,
    SidebarMenuItem,
    SidebarMenuSub,
    SidebarMenuSubButton,
    SidebarMenuSubItem,
} from '@/components/ui/sidebar';
import { agent } from '@/routes';

type PrimaryItem = { title: string; icon: LucideIcon; href?: string };

const primaryItems: PrimaryItem[] = [
    { title: 'New chat', icon: SquarePen, href: agent().url },
    { title: 'Scheduled', icon: CalendarClock },
    { title: 'Graph', icon: Network },
    { title: 'Extensions', icon: Plug },
];

const projects = [
    { name: 'Design system', chats: ['Token audit', 'Button variants'] },
    { name: 'Client portal', chats: ['Onboarding flow'] },
    { name: 'Marketing website', chats: [] },
    {
        name: 'Platform',
        chats: ['API migration', 'Billing states', 'Read platform docs'],
    },
    { name: 'Research lab', chats: [] },
];

export function AppSidebar() {
    return (
        <Sidebar collapsible="icon" variant="sidebar">
            <SidebarHeader>
                <SidebarMenu>
                    <SidebarMenuItem>
                        <SidebarMenuButton size="lg" asChild>
                            <Link href={agent()} prefetch>
                                <AppLogo />
                            </Link>
                        </SidebarMenuButton>
                    </SidebarMenuItem>
                </SidebarMenu>
            </SidebarHeader>

            <SidebarContent>
                <SidebarGroup>
                    <SidebarMenu>
                        {primaryItems.map((item) => (
                            <SidebarMenuItem key={item.title}>
                                <SidebarMenuButton
                                    asChild={!!item.href}
                                    tooltip={item.title}
                                >
                                    {item.href ? (
                                        <Link href={item.href} prefetch>
                                            <item.icon />
                                            <span>{item.title}</span>
                                        </Link>
                                    ) : (
                                        <>
                                            <item.icon />
                                            <span>{item.title}</span>
                                        </>
                                    )}
                                </SidebarMenuButton>
                            </SidebarMenuItem>
                        ))}
                    </SidebarMenu>
                </SidebarGroup>

                <SidebarGroup className="group-data-[collapsible=icon]:hidden">
                    <SidebarGroupLabel>Projects</SidebarGroupLabel>
                    <SidebarMenu>
                        {projects.map((project) => (
                            <Collapsible
                                key={project.name}
                                asChild
                                defaultOpen={project.name === 'Platform'}
                                className="group/collapsible"
                            >
                                <SidebarMenuItem>
                                    <CollapsibleTrigger asChild>
                                        <SidebarMenuButton>
                                            <Folder className="group-data-[state=open]/collapsible:hidden" />
                                            <FolderOpen className="hidden group-data-[state=open]/collapsible:block" />
                                            <span>{project.name}</span>
                                            <ChevronRight className="ml-auto transition-transform group-data-[state=open]/collapsible:rotate-90" />
                                        </SidebarMenuButton>
                                    </CollapsibleTrigger>
                                    <CollapsibleContent>
                                        <SidebarMenuSub>
                                            {project.chats.length === 0 && (
                                                <SidebarMenuSubItem>
                                                    <span className="px-2 text-xs text-muted-foreground">
                                                        No chats yet
                                                    </span>
                                                </SidebarMenuSubItem>
                                            )}
                                            {project.chats.map((chat) => (
                                                <SidebarMenuSubItem key={chat}>
                                                    <SidebarMenuSubButton asChild>
                                                        <Link href={agent()}>
                                                            <MessageSquare />
                                                            <span>{chat}</span>
                                                        </Link>
                                                    </SidebarMenuSubButton>
                                                </SidebarMenuSubItem>
                                            ))}
                                        </SidebarMenuSub>
                                    </CollapsibleContent>
                                </SidebarMenuItem>
                            </Collapsible>
                        ))}
                    </SidebarMenu>
                </SidebarGroup>
            </SidebarContent>

            <SidebarFooter>
                <NavUser />
            </SidebarFooter>
        </Sidebar>
    );
}
