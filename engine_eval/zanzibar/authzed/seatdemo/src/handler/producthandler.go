package handler

import (
	"context"
	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/authzed/authzed-go/v1"
	"github.com/labstack/echo/v4"
	"io"
	"log"
	"net/http"
	"strconv"
)

var licenseMap = map[string]int{"p1": 5, "p2": 3}

type ProductLicense struct {
	Name   string `json:"name" xml:"name"`
	Active int    `json:"active_licenses" xml:"active_licenses"`
	Max    int    `json:"max_seats" xml:"max_seats"`
}

type GrantLicenseResponse struct {
	Granted bool   `json:"granted"`
	Message string `json:"reason"`
}

type GrantLicenseRequest struct {
	UserId string `form:"userId"`
}

func GetLicenseInfoForProductInstance(c echo.Context) error {
	ctx := context.Background()
	if port == "" {
		port = "50051" //TODO
	}
	client, err := getSpiceDbApiClient(port)

	if err != nil {
		log.Fatalf("unable to initialize client: %s", err)
		return err
	}

	callingName := c.QueryParam("callingName")

	subject := &v1.SubjectReference{Object: &v1.ObjectReference{
		ObjectType: "user",
		ObjectId:   callingName,
	}}

	tId := c.Param("tenant")
	t1 := &v1.ObjectReference{
		ObjectType: "tenant",
		ObjectId:   tId,
	}

	resp, err2 := client.CheckPermission(ctx, &v1.CheckPermissionRequest{
		Resource:   t1,
		Permission: "manage_seats",
		Subject:    subject,
	})

	if err2 != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "uh oh...")
	}

	//check permissions: is p1 tied to customer1? actual intent: is user allowed to access p1 licensing data? let's check manage_seats permission on tenant resource for now.
	if resp.Permissionship != v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION {
		return echo.NewHTTPError(http.StatusForbidden, "You are not allowed to see licensing information. manage_seats is required (and too coarse grained, but for the sake of example it suffices.")
	}

	pInstance := c.Param("pinstance")
	//TODO: check if maxseat value for pInstance exist against map first, if not in map, bad request -> test.
	currentLicenseCount, err3 := GetCurrentActiveLicenseCountForProductInstance(pInstance, client, ctx)
	if err3 != nil {
		return echo.NewHTTPError(http.StatusForbidden, "Internal Server error occured. Please try again later.")
	}

	result := ProductLicense{
		Name:   pInstance,
		Active: currentLicenseCount,
		Max:    licenseMap[pInstance],
	}
	return c.JSON(http.StatusOK, result)
}

func GrantLicenseIfNotFull(c echo.Context) error {
	ctx := context.Background()
	if port == "" {
		port = "50051" //TODO: remove and refactor
	}

	//sanity check if requestbody contains userid. send 400 if empty.
	var grReq GrantLicenseRequest
	err := c.Bind(&grReq)

	if err != nil || grReq.UserId == "" { //TODO: evaluate why binding in code does not return error when this is empty...
		return echo.NewHTTPError(http.StatusBadRequest, "Bad Request. User to grant access to needed")
	}

	client, err2 := getSpiceDbApiClient(port)

	if err2 != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "Internal Server error.")
	}

	// TODO: this and the following check should be consolidated into one CheckPermissions, change model accordingly, should work, but not now..
	//check for tenant membership of user to grant stuff for.
	subj := &v1.SubjectReference{Object: &v1.ObjectReference{
		ObjectType: "user",
		ObjectId:   grReq.UserId,
	}}

	tId := c.Param("tenant")
	tenant := &v1.ObjectReference{
		ObjectType: "tenant",
		ObjectId:   tId,
	}

	resp, err3 := client.CheckPermission(ctx, &v1.CheckPermissionRequest{
		Resource:   tenant,
		Permission: "membership",
		Subject:    subj,
	})

	if err3 != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "uh oh..")
	}

	//check permissions: is user member of tenant?
	if resp.Permissionship != v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION {
		return echo.NewHTTPError(http.StatusForbidden, "User "+grReq.UserId+" is not a member of licensed tenant "+tId)
	}

	subject := &v1.SubjectReference{Object: &v1.ObjectReference{
		ObjectType: "user",
		ObjectId:   grReq.UserId,
	}}

	t2 := &v1.ObjectReference{
		ObjectType: "user",
		ObjectId:   grReq.UserId,
	}

	r, err4 := client.CheckPermission(ctx, &v1.CheckPermissionRequest{
		Resource:   t2,
		Permission: "is_not_activated_wsdm_user",
		Subject:    subject,
	})

	if err4 != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "uh oh.. ")
	}

	//check permissions: is user not already activated wsdm user?!
	if r.Permissionship != v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION {
		return echo.NewHTTPError(http.StatusConflict, "Already active license for user "+grReq.UserId+" found.")
	}
	pInstance := c.Param("pinstance")
	isFull, currentCount, err5 := isLicenseFull(pInstance, c, client, ctx)

	if err5 != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "Internal Server error.")
	}

	if isFull {
		result := GrantLicenseResponse{
			Granted: false,
			Message: "Maximum seats exceeded. Please extend your license.",
		}
		//let's have day long discussions around HTTP status codes pls ;)
		return c.JSON(http.StatusConflict, result)
	}

	updates := []*v1.RelationshipUpdate{
		{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{
					ObjectType: "user",
					ObjectId:   grReq.UserId,
				},
				Relation: "licensed_wsdm_user",
				Subject: &v1.SubjectReference{
					Object: &v1.ObjectReference{
						ObjectType: "user",
						ObjectId:   grReq.UserId,
					},
				},
			},
		}, {
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{
					ObjectType: "product_instance",
					ObjectId:   pInstance,
				},
				Relation: "wsdm_user",
				Subject: &v1.SubjectReference{
					Object: &v1.ObjectReference{
						ObjectType: "user",
						ObjectId:   grReq.UserId,
					},
				},
			},
		},
	}

	//grant license: set is_activated_wsdm_user to user from postbody, and add relation to product_instance from path
	_, wrErr := client.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates:               updates,
		OptionalPreconditions: nil,
	})

	if wrErr != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "uh oh.. "+wrErr.Error())
	}

	result := GrantLicenseResponse{
		Granted: true,
		Message: "Successfully granted a license for instance " + pInstance + " to " + grReq.UserId + ". Remaining: " + strconv.Itoa(licenseMap[pInstance]-currentCount-1),
	}
	return c.JSON(http.StatusCreated, result)
}

// sanity check if current < max licenses for tenant. Send 403 + reason: license full.
func isLicenseFull(pInstance string, c echo.Context, client *authzed.Client, ctx context.Context) (bool, int, error) {
	currCount, err := GetCurrentActiveLicenseCountForProductInstance(pInstance, client, ctx)
	if err != nil {
		return false, currCount, err
	}

	return currCount >= licenseMap[pInstance], currCount, nil
}

func GetCurrentActiveLicenseCountForProductInstance(pInstance string, client *authzed.Client, ctx context.Context) (int, error) {
	productInstance := &v1.ObjectReference{
		ObjectType: "product_instance",
		ObjectId:   pInstance,
	}

	//if yes: get is_active_user LookupSubject for product_instance, count and put into active.
	stream, err := client.LookupSubjects(ctx, &v1.LookupSubjectsRequest{
		Resource:          productInstance,
		Permission:        "is_active_user",
		SubjectObjectType: "user",
	})

	if err != nil {
		log.Fatal("error fetching licenses using LookupSubject", err)
		return -1, err
	}

	var currentLicenseCount int
	for { //most likely weird and ugly, but works.
		_, err := stream.Recv()
		if err == io.EOF {
			break
		} else if err == nil {
			//just count.
			currentLicenseCount++
		}

		if err != nil {
			log.Fatal("error fetching current licenses", err)
			return -1, err
		}
	}
	return currentLicenseCount, nil
}
