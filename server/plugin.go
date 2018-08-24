package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/mattermost/mattermost-server/mlog"
	"github.com/mattermost/mattermost-server/model"
	"github.com/mattermost/mattermost-server/plugin"

	"github.com/xanzy/go-gitlab"
	"golang.org/x/oauth2"
)

const (
	GITLAB_TOKEN_KEY        = "_gitlabtoken"
	GITLAB_STATE_KEY        = "_gitlabstate"
	GITLAB_USERNAME_KEY     = "_gitlabusername"
	WS_EVENT_CONNECT        = "connect"
	WS_EVENT_DISCONNECT     = "disconnect"
	WS_EVENT_REFRESH        = "refresh"
	SETTING_BUTTONS_TEAM    = "team"
	SETTING_BUTTONS_CHANNEL = "channel"
	SETTING_BUTTONS_OFF     = "off"
	SETTING_NOTIFICATIONS   = "notifications"
	SETTING_REMINDERS       = "reminders"
	SETTING_ON              = "on"
	SETTING_OFF             = "off"
)

type Plugin struct {
	plugin.MattermostPlugin
	gitlabClient *gitlab.Client

	BotUserID string

	GitLabOrg               string
	Username                string
	GitLabOAuthClientID     string
	GitLabOAuthClientSecret string
	WebhookSecret           string
	EncryptionKey           string
	BaseURL                 string
	UploadURL               string
}

func (p *Plugin) gitlabConnect(token oauth2.Token) *gitlab.Client {
	client := gitlab.NewOAuthClient(nil, token.AccessToken)
	return client
}

func (p *Plugin) OnActivate() error {
	if err := p.IsValid(); err != nil {
		return err
	}
	p.API.RegisterCommand(getCommand())
	user, err := p.API.GetUserByUsername(p.Username)
	if err != nil {
		mlog.Error(err.Error())
		return fmt.Errorf("Unable to find user with configured username: %v", p.Username)
	}

	p.BotUserID = user.Id
	return nil
}

func (p *Plugin) IsValid() error {
	if p.GitLabOAuthClientID == "" {
		return fmt.Errorf("Must have a gitlab oauth client id")
	}

	if p.GitLabOAuthClientSecret == "" {
		return fmt.Errorf("Must have a gitlab oauth client secret")
	}

	if p.EncryptionKey == "" {
		return fmt.Errorf("Must have an encryption key")
	}

	if p.Username == "" {
		return fmt.Errorf("Need a user to make posts as")
	}

	if p.BaseURL == "" {
		return fmt.Errorf("Must have a gitlab base url")
	}

	if p.UploadURL == "" {
		return fmt.Errorf("Must have a gitlab upload url")
	}

	return nil
}

func (p *Plugin) getOAuthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.GitLabOAuthClientID,
		ClientSecret: p.GitLabOAuthClientSecret,
		Scopes:       []string{"api","read_user"},
		RedirectURL:  fmt.Sprintf("%s/plugins/gitlab/oauth/complete", *p.API.GetConfig().ServiceSettings.SiteURL),
		Endpoint: oauth2.Endpoint{
			AuthURL:  p.BaseURL + "/oauth/authorize",
			TokenURL: p.BaseURL + "/oauth/token",
		},
	}
}

type GitLabUserInfo struct {
	UserID         string
	Token          *oauth2.Token
	GitLabUsername string
	LastToDoPostAt int64
	Settings       *UserSettings
}

type UserSettings struct {
	SidebarButtons string `json:"sidebar_buttons"`
	DailyReminder  bool   `json:"daily_reminder"`
	Notifications  bool   `json:"notifications"`
}

func (p *Plugin) storeGitLabUserInfo(info *GitLabUserInfo) error {
	encryptedToken, err := encrypt([]byte(p.EncryptionKey), info.Token.AccessToken)
	if err != nil {
		return err
	}

	info.Token.AccessToken = encryptedToken

	jsonInfo, err := json.Marshal(info)
	if err != nil {
		return err
	}

	if err := p.API.KVSet(info.UserID+GITLAB_TOKEN_KEY, jsonInfo); err != nil {
		return err
	}

	return nil
}

func (p *Plugin) getGitLabUserInfo(userID string) (*GitLabUserInfo, *APIErrorResponse) {
	var userInfo GitLabUserInfo

	if infoBytes, err := p.API.KVGet(userID + GITLAB_TOKEN_KEY); err != nil || infoBytes == nil {
		return nil, &APIErrorResponse{ID: API_ERROR_ID_NOT_CONNECTED, Message: "Must connect user account to GitLab first.", StatusCode: http.StatusBadRequest}
	} else if err := json.Unmarshal(infoBytes, &userInfo); err != nil {
		return nil, &APIErrorResponse{ID: "", Message: "Unable to parse token.", StatusCode: http.StatusInternalServerError}
	}

	unencryptedToken, err := decrypt([]byte(p.EncryptionKey), userInfo.Token.AccessToken)
	if err != nil {
		mlog.Error(err.Error())
		return nil, &APIErrorResponse{ID: "", Message: "Unable to decrypt access token.", StatusCode: http.StatusInternalServerError}
	}

	userInfo.Token.AccessToken = unencryptedToken

	return &userInfo, nil
}

func (p *Plugin) storeGitLabToUserIDMapping(gitlabUsername, userID string) error {
	if err := p.API.KVSet(gitlabUsername+GITLAB_USERNAME_KEY, []byte(userID)); err != nil {
		return fmt.Errorf("Encountered error saving gitlab username mapping")
	}
	return nil
}

func (p *Plugin) getGitLabToUserIDMapping(gitlabUsername string) string {
	userID, _ := p.API.KVGet(gitlabUsername + GITLAB_USERNAME_KEY)
	return string(userID)
}

func (p *Plugin) disconnectGitLabAccount(userID string) {
	userInfo, _ := p.getGitLabUserInfo(userID)
	if userInfo == nil {
		return
	}

	p.API.KVDelete(userID + GITLAB_TOKEN_KEY)
	p.API.KVDelete(userInfo.GitLabUsername + GITLAB_USERNAME_KEY)

	if user, err := p.API.GetUser(userID); err == nil && user.Props != nil && len(user.Props["gitlab_user"]) > 0 {
		delete(user.Props, "gitlab_user")
		p.API.UpdateUser(user)
	}

	p.API.PublishWebSocketEvent(
		WS_EVENT_DISCONNECT,
		nil,
		&model.WebsocketBroadcast{UserId: userID},
	)
}

func (p *Plugin) CreateBotDMPost(userID, message, postType string) *model.AppError {
	channel, err := p.API.GetDirectChannel(userID, p.BotUserID)
	if err != nil {
		mlog.Error("Couldn't get bot's DM channel", mlog.String("user_id", userID))
		return err
	}

	post := &model.Post{
		UserId:    p.BotUserID,
		ChannelId: channel.Id,
		Message:   message,
		Type:      postType,
		Props: map[string]interface{}{
			"from_webhook":      "true",
			"override_username": GITLAB_USERNAME,
			"override_icon_url": GITLAB_ICON_URL,
		},
	}

	if _, err := p.API.CreatePost(post); err != nil {
		mlog.Error(err.Error())
		return err
	}

	return nil
}

func (p *Plugin) PostToDo(info *GitLabUserInfo) {
	text, err := p.GetToDo(context.Background(), info.GitLabUsername, p.gitlabConnect(*info.Token))
	if err != nil {
		mlog.Error(err.Error())
		return
	}

	p.CreateBotDMPost(info.UserID, text, "custom_gitlab_todo")
}

func (p *Plugin) GetToDo(ctx context.Context, username string, gitlabClient *gitlab.Client) (string, error) {
	todos, _, err := gitlabClient.Todos.ListTodos(&gitlab.ListTodosOptions{})
	if err != nil {
		return "", err
	}

	mergeRequests, _, err := gitlabClient.MergeRequests.ListMergeRequests(&gitlab.ListMergeRequestsOptions{
		Scope: gitlab.String("created_by_me"),
	})
	if err != nil {
		return "", err
	}

	assignedIssues, _, err := gitlabClient.Issues.ListIssues(&gitlab.ListIssuesOptions{
		Scope: gitlab.String("assigned_to_me"),
	})
	if err != nil {
		return "", err
	}
	assignedMrs, _, err := gitlabClient.MergeRequests.ListMergeRequests(&gitlab.ListMergeRequestsOptions{
		Scope: gitlab.String("assigned_to_me"),
	})
	if err != nil {
		return "", err
	}

	text := "##### Todos\n"

	todoCount := 0
	todoContent := ""
	for _, todo := range todos {
		if todo.State == "done" {
			continue
		}

		if &todo.Project == nil {
			p.API.LogError("Unable to get repository for notification in todo list. Skipping.")
			continue
		}

		_, org, _ := parseOwnerAndRepo(todo.Project.PathWithNamespace, p.BaseURL)
		if p.checkOrg(org) != nil {
			continue
		}

		todoContent += fmt.Sprintf("* %v\n", todo.TargetURL)
		todoCount++
	}

	if todoCount == 0 {
		text += "You don't have any todos.\n"
	} else {
		text += fmt.Sprintf("You have %v pending todos:\n", todoCount)
		text += todoContent
	}

	text += "##### Your Open Merge Requests\n"

	if len(mergeRequests) == 0 {
		text += "You have don't have any open merge requests.\n"
	} else {
		text += fmt.Sprintf("You have %v open merge requests:\n", len(mergeRequests))

		for _, mr := range mergeRequests {
			text += fmt.Sprintf("* %v\n", mr.WebURL)
		}
	}

	text += "##### Your Assigments\n"

	if len(assignedIssues) == 0 && len(assignedMrs) == 0 {
		text += "You have don't have any assignments.\n"
	} else {
		text += fmt.Sprintf("You have %v assignments:\n", (len(assignedIssues)+len(assignedMrs)))

		for _, issue := range assignedIssues {
			text += fmt.Sprintf("* %v\n", issue.WebURL)
		}
		for _, mr := range assignedMrs {
			text += fmt.Sprintf("* %v\n", mr.WebURL)
		}
	}

	return text, nil
}

func (p *Plugin) checkOrg(org string) error {
	configOrg := strings.TrimSpace(p.GitLabOrg)
	if configOrg != "" && configOrg != org {
		return fmt.Errorf("Only repositories in the %v organization are supported", configOrg)
	}

	return nil
}

func (p *Plugin) sendRefreshEvent(userID string) {
	p.API.PublishWebSocketEvent(
		WS_EVENT_REFRESH,
		nil,
		&model.WebsocketBroadcast{UserId: userID},
	)
}
